package main

import (
	"context"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Albatrossvpn/vp/internal/antiblock"
	"github.com/Albatrossvpn/vp/internal/node"
)

// The proven list (proven.txt) is what accounts are given (backend/src/
// sub.js): the hot servers with fresh evidence from inside Iran that they
// are healthy there. Nothing else reaches a user.
//
// Every hot server passed the collector's real test (realtest.go): two
// connections through it, each carrying a real download, and what the
// reports from inside (crowd.go) say fails there was left out. That test
// runs abroad. On top of it, a proven server needs evidence from inside
// the country, younger than provenFresh, of one of three kinds:
//
//   - tester: a tester on an Iranian line (deploy/tester) connected
//     through the server for real, twice, with a download each time, and
//     confirmed it (iran.json, written by the collector there with the
//     same real test and its bars). That proves the speed from Iran too.
//     It is the only evidence for a server over UDP (Hysteria2, TUIC,
//     WireGuard), which no probe opens a TCP connection to.
//   - globalping: Globalping's probes inside Iran (globalping.go), the
//     project's own first, opened TCP connections to the server's address:
//     gpMinProbes probes at least, every handshake answered, quickly
//     (provenMaxRTT) and evenly (provenMaxJitter), and the check before
//     this one found it open too, so it stays up. The speed comes from
//     the collector's real test (provenMinSpeed).
//   - cdn: a server reached through Cloudflare (cdnFronted) is reached by
//     the apps on whichever clean Cloudflare address the network lets
//     through, not on the address its link names, so no probe can test it
//     from inside. It is proven when reach.json lists provenCleanEdges
//     clean addresses open from inside (the way in) and the server passed
//     the real test at provenMinSpeed (it carries behind the CDN).
//
// Either vantage point stands alone, so each covers for the other: with
// the tester down Globalping still proves, with Globalping down the
// tester's report does. With neither, nothing is proven and proven.txt is
// empty; the apps still have WARP, Psiphon, Tor and, on the plans that
// have it, the edge.
const (
	// provenFresh: evidence older than this proves nothing.
	provenFresh = 3 * time.Hour
	// provenMinSpeed: the slower of the two downloads of the real test,
	// bytes a second (for a server the tester did not confirm).
	provenMinSpeed = 512 << 10
	// provenMaxRTT: the mean time of the TCP handshakes from inside, ms.
	provenMaxRTT = 300
	// provenMaxJitter: the mean difference between a probe's consecutive
	// handshakes, ms.
	provenMaxJitter = 40
	// provenMaxLoss: handshakes without an answer, percent: none at all.
	provenMaxLoss = 0
	// provenChecks: the address was open in each of the last this many
	// checks from inside, each at most provenGap after the one before.
	provenChecks = 2
	provenGap    = 6 * time.Hour
	// provenCleanEdges: clean Cloudflare addresses open from inside
	// (reach.json) that prove the way in to the servers behind Cloudflare.
	provenCleanEdges = 2
)

// provenBands rank proven servers by the real test's download, fastest
// band first, so the servers that carried 1 MB/s or more lead; within a
// band, the shorter round trip from inside goes first (servers without
// one, over UDP or behind Cloudflare, after those with one), then the
// shorter delay of the real test.
var provenBands = []float64{4 << 20, 2 << 20, 1 << 20}

// provenEvidence is what is known from inside the country.
type provenEvidence struct {
	now time.Time
	// tester is the freshest tester report (crowd.go).
	tester testerReport
	// answers is Globalping's, by address (iran-tcp.json, checkIran).
	answers map[string]iranSeen
	// cdn marks the servers reached through Cloudflare (cdnFronted).
	cdn map[*entry]bool
}

// provenResult is the proven list, best first, and what proved it.
type provenResult struct {
	list []*entry
	// by names the evidence that was current: "tester", "globalping",
	// both joined with "+", or "none".
	by string
	// from counts the servers each kind of evidence proved.
	from map[string]int
}

// decideProven picks from the hot servers (cands, one per address, with
// the delays of the real test in passed) those with fresh evidence from
// inside that they are healthy, at most max, best first.
func decideProven(cands []*entry, passed map[*entry]time.Duration, ev provenEvidence, max int) provenResult {
	fresh := func(at time.Time) bool { return !at.IsZero() && ev.now.Sub(at) <= provenFresh }
	testerOn := fresh(ev.tester.at)
	gpOn := false
	for _, c := range ev.answers {
		if fresh(time.Unix(c.At, 0)) {
			gpOn = true
			break
		}
	}
	edges := len(cleanEdges(ev.answers, ev.now))
	type pick struct {
		e    *entry
		kind string
		rtt  float64 // the round trip from inside, ms; -1 when unknown
	}
	var picks []pick
	for _, e := range cands {
		kind, rtt := "", -1.0
		c, checked := ev.answers[e.endpoint]
		if checked && e.endpoint != "" && !ev.cdn[e] && c.Probes > 0 && fresh(time.Unix(c.At, 0)) {
			rtt = c.RTT
		}
		switch {
		case testerOn && ev.tester.confirmed[e.key]:
			kind = "tester"
		case e.endpoint == "":
			// Over UDP: only a tester can open a connection to it.
		case e.speed < provenMinSpeed:
		case ev.cdn[e]:
			if edges >= provenCleanEdges {
				kind = "cdn"
			}
		case checked && gpHealthy(c, ev.now):
			kind = "globalping"
		}
		if kind != "" {
			picks = append(picks, pick{e, kind, rtt})
		}
	}
	band := func(e *entry) int {
		for i, b := range provenBands {
			if e.speed >= b {
				return i
			}
		}
		return len(provenBands)
	}
	sort.SliceStable(picks, func(i, j int) bool {
		a, b := picks[i], picks[j]
		if ba, bb := band(a.e), band(b.e); ba != bb {
			return ba < bb
		}
		if ka, kb := a.rtt >= 0, b.rtt >= 0; ka != kb {
			return ka
		}
		if a.rtt != b.rtt {
			return a.rtt < b.rtt
		}
		return passed[a.e] < passed[b.e]
	})
	r := provenResult{from: map[string]int{}, by: "none"}
	for _, p := range picks[:min(len(picks), max)] {
		r.list = append(r.list, p.e)
		r.from[p.kind]++
	}
	var on []string
	if testerOn {
		on = append(on, "tester")
	}
	if gpOn {
		on = append(on, "globalping")
	}
	if len(on) > 0 {
		r.by = strings.Join(on, "+")
	}
	log.Printf("proven: %d of %d hot servers (%d by the tester's report, %d by Globalping, %d behind Cloudflare with %d clean edge addresses open); evidence current: %s",
		len(r.list), len(cands), r.from["tester"], r.from["globalping"], r.from["cdn"], edges, r.by)
	return r
}

// gpHealthy reports whether Globalping's answers about an address prove
// it healthy from inside the country: a fresh answer from gpMinProbes
// probes or more with every handshake answered, fast and even enough, and
// open in the checks before it too.
func gpHealthy(c iranSeen, now time.Time) bool {
	if !c.Open || now.Sub(time.Unix(c.At, 0)) > provenFresh {
		return false
	}
	if c.Probes < gpMinProbes || c.RTT <= 0 || c.RTT > provenMaxRTT || c.Jitter < 0 || c.Jitter > provenMaxJitter || c.Loss > provenMaxLoss {
		return false
	}
	if len(c.Hist) < provenChecks-1 {
		return false
	}
	last := c.At
	for _, h := range c.Hist[:provenChecks-1] {
		if !h.Open || time.Unix(last, 0).Sub(time.Unix(h.At, 0)) > provenGap {
			return false
		}
		last = h.At
	}
	return true
}

// lookupHost resolves a name (a variable so tests can answer for it).
var lookupHost = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// cdnFronted marks the servers reached through Cloudflare: a transport any
// Cloudflare edge carries (WebSocket, HTTPUpgrade, XHTTP, gRPC) over TLS,
// on a port Cloudflare proxies, with a name to route on
// (antiblock.CDNFrontable), and on Cloudflare: the link's address is one
// of Cloudflare's, or that name resolves to one. The apps reach these on a
// clean Cloudflare address when the link's own does not answer
// (internal/engine/scan.go), so what a probe sees of the link's address
// says nothing about them. A name that does not resolve leaves its servers
// unmarked: they are checked like any other.
func cdnFronted(es []*entry) map[*entry]bool {
	out := map[*entry]bool{}
	names := map[string][]*entry{}
	for _, e := range es {
		n, err := node.ParseLink(e.link)
		if err != nil || n.Security != "tls" || !antiblock.CDNFrontable(n) {
			continue
		}
		if antiblock.IsCloudflare(n.Server) {
			out[e] = true
		} else if d := antiblock.Domain(n); d != "" {
			names[d] = append(names[d], e)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for name, list := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func(name string, list []*entry) {
			defer func() { <-sem; wg.Done() }()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			addrs, err := lookupHost(ctx, name)
			if err != nil {
				return
			}
			for _, a := range addrs {
				if antiblock.IsCloudflare(a) {
					mu.Lock()
					for _, e := range list {
						out[e] = true
					}
					mu.Unlock()
					return
				}
			}
		}(name, list)
	}
	wg.Wait()
	return out
}
