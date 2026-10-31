package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// globalpingAPI runs measurements from volunteer probes around the world,
// free and without an account (GLOBALPING_TOKEN, when set, raises the
// hourly quota).
var globalpingAPI = "https://api.globalping.io/v1/measurements"

// gpPoll is how often a measurement is asked for its results.
var gpPoll = 500 * time.Millisecond

// iranReach asks probes inside Iran to open TCP connections to each
// address, gpPackets times, and reports what they saw (gpAnswer). The
// collector's own test runs from the US and cannot see Iran's filtering;
// this can. Addresses missing from the result were not checked (no probe
// free, quota used up, no probe could run the test). Each address is
// tested by gpMinProbes probes: the project's own (GLOBALPING_TAGS) first,
// the volunteers' in the country filling the rest (gpLocations), so the
// check never hangs on one machine being off.
func iranReach(ctx context.Context, endpoints []string) map[string]gpAnswer {
	out := map[string]gpAnswer{}
	var mu sync.Mutex
	stop := make(chan struct{})
	var once, fell sync.Once
	var own atomic.Bool
	own.Store(gpOwnTags() != nil)
	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	for _, ep := range endpoints {
		select {
		case <-stop:
		case sem <- struct{}{}:
			wg.Add(1)
			go func(ep string) {
				defer func() { <-sem; wg.Done() }()
				useOwn := own.Load()
				a, err := tcpFromIran(ctx, ep, useOwn)
				if err == errNoProbes && useOwn {
					// The API refused the own probes' location outright
					// (a tag it does not take): the volunteers alone.
					fell.Do(func() {
						log.Print("globalping: the own probes could not be asked; using the volunteers' probes in Iran")
					})
					own.Store(false)
					a, err = tcpFromIran(ctx, ep, false)
				}
				if err != nil {
					if err == errNoProbes || err == errQuota {
						once.Do(func() { log.Printf("globalping: %v", err); close(stop) })
					}
					return
				}
				mu.Lock()
				out[ep] = a
				mu.Unlock()
			}(ep)
			continue
		}
		break
	}
	wg.Wait()
	return out
}

// gpOwnTags returns the tags of the project's own adopted probes inside
// the country (GLOBALPING_TAGS, comma separated, as the dashboard shows
// them, like u-albatrossvpn), or nil when there are none.
func gpOwnTags() []string {
	var list []string
	for _, t := range strings.Split(os.Getenv("GLOBALPING_TAGS"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			list = append(list, t)
		}
	}
	return list
}

// gpMinProbes is how many probes inside the country test each address. One
// probe speaks for one network only: an address counts as open from Iran
// for the proven list when at least this many completed handshakes with it
// (proven.go). Each probe's test is one test of the hourly quota.
const gpMinProbes = 2

// gpPackets is how many TCP handshakes each probe makes and times: the
// spread between consecutive ones is the jitter.
const gpPackets = 4

// gpLocations picks the probes, all inside Iran: with own probes, those
// first and the volunteers' after. The API shares the limit out between
// the two locations, and the share the own probes cannot fill (one probe,
// or none online) goes to the volunteers, so every address is still tested
// by gpMinProbes probes; a probe both locations match is picked once.
func gpLocations(own bool) []any {
	if tags := gpOwnTags(); own && tags != nil {
		return []any{map[string]any{"country": "IR", "tags": tags}, map[string]any{"country": "IR"}}
	}
	return []any{map[string]any{"country": "IR"}}
}

var (
	errNoProbes = fmt.Errorf("no probes in Iran right now")
	errQuota    = fmt.Errorf("hourly quota used up")
	errNoAnswer = fmt.Errorf("no probe could run the test")
)

// gpAnswer is what the probes inside the country saw of one address.
type gpAnswer struct {
	open   bool    // at least one probe completed a handshake
	probes int     // how many probes completed one
	rtt    float64 // the mean time of the handshakes that completed, ms
	// jitter is the mean difference between a probe's consecutive
	// handshakes, averaged over the probes that timed two or more, ms;
	// -1 when none did.
	jitter float64
	// loss is the share of handshakes that got no answer, percent of all
	// that the probes that ran the test made.
	loss float64
}

func tcpFromIran(ctx context.Context, endpoint string, own bool) (gpAnswer, error) {
	host, portS, err := net.SplitHostPort(endpoint)
	if err != nil {
		return gpAnswer{}, err
	}
	port, _ := strconv.Atoi(portS)
	body, _ := json.Marshal(map[string]any{
		"type":               "ping",
		"target":             host,
		"locations":          gpLocations(own),
		"limit":              gpMinProbes,
		"measurementOptions": map[string]any{"packets": gpPackets, "protocol": "TCP", "port": port},
	})
	var created struct {
		ID string `json:"id"`
	}
	status, err := gpDo(ctx, http.MethodPost, globalpingAPI, body, &created)
	switch {
	case status == http.StatusUnprocessableEntity:
		return gpAnswer{}, errNoProbes
	case status == http.StatusTooManyRequests:
		return gpAnswer{}, errQuota
	case err != nil:
		return gpAnswer{}, err
	}
	var m gpMeasurement
	for i := 0; i < 60; i++ {
		time.Sleep(gpPoll)
		if _, err := gpDo(ctx, http.MethodGet, globalpingAPI+"/"+created.ID, nil, &m); err != nil {
			return gpAnswer{}, err
		}
		if m.Status != "in-progress" {
			break
		}
	}
	if m.Status != "finished" || len(m.Results) == 0 {
		return gpAnswer{}, fmt.Errorf("measurement %s", m.Status)
	}
	return m.answer()
}

// gpMeasurement is a finished ping measurement as the API reports it: per
// probe, each handshake's time (timings) and the counts (stats).
type gpMeasurement struct {
	Status  string `json:"status"`
	Results []struct {
		Result struct {
			Status        string `json:"status"`
			FailureSource string `json:"failureSource"`
			Timings       []struct {
				RTT float64 `json:"rtt"`
			} `json:"timings"`
			Stats struct {
				Total int      `json:"total"`
				Rcv   int      `json:"rcv"`
				Avg   *float64 `json:"avg"`
			} `json:"stats"`
		} `json:"result"`
	} `json:"results"`
}

// answer sums up the probes' results. A probe that got an answer says the
// address is open there. One that ran and got none, or failed at the
// target, says it is closed there, and its handshakes count as lost. One
// that failed on its own side (internal), or never finished, says nothing:
// a probe going off mid-run must not close every address it had.
func (m *gpMeasurement) answer() (gpAnswer, error) {
	var a gpAnswer
	said := false
	sent, got := 0, 0
	var rttSum, jitSum float64
	rttN, jitN := 0, 0
	for _, r := range m.Results {
		res := r.Result
		switch {
		case res.Status == "finished":
			said = true
			total := res.Stats.Total
			if total <= 0 {
				total = gpPackets
			}
			sent += total
			got += min(res.Stats.Rcv, total)
			if res.Stats.Rcv <= 0 {
				continue
			}
			a.probes++
			var times []float64
			for _, t := range res.Timings {
				if t.RTT > 0 {
					times = append(times, t.RTT)
				}
			}
			switch {
			case len(times) > 0:
				for _, t := range times {
					rttSum += t
				}
				rttN += len(times)
			case res.Stats.Avg != nil:
				rttSum += *res.Stats.Avg * float64(res.Stats.Rcv)
				rttN += res.Stats.Rcv
			}
			if len(times) >= 2 {
				jitSum += meanStep(times)
				jitN++
			}
		case res.Status == "failed" && res.FailureSource != "internal":
			said = true
			sent += gpPackets
		}
	}
	if !said {
		return gpAnswer{}, errNoAnswer
	}
	a.open = a.probes > 0
	if rttN > 0 {
		a.rtt = rttSum / float64(rttN)
	}
	a.jitter = -1
	if jitN > 0 {
		a.jitter = jitSum / float64(jitN)
	}
	if sent > 0 {
		a.loss = 100 * float64(sent-got) / float64(sent)
	}
	return a, nil
}

// meanStep is the mean absolute difference between consecutive values:
// the jitter of a series of round trips.
func meanStep(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	sum := 0.0
	for i := 1; i < len(v); i++ {
		sum += math.Abs(v[i] - v[i-1])
	}
	return sum / float64(len(v)-1)
}

func gpDo(ctx context.Context, method, url string, body []byte, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t := os.Getenv("GLOBALPING_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

// iranFirst orders hot servers by what the Iranian probes saw: open there
// first, then unchecked, then those that did not answer. Nothing is
// dropped, since a few probes cannot speak for every operator.
func iranFirst(hot []*entry, seen map[string]bool) []*entry {
	rank := func(e *entry) int {
		open, ok := seen[e.endpoint]
		switch {
		case ok && open:
			return 0
		case !ok:
			return 1
		}
		return 2
	}
	sort.SliceStable(hot, func(i, j int) bool { return rank(hot[i]) < rank(hot[j]) })
	return hot
}
