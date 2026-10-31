package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGlobalping answers measurements with the result set for each target
// and records the limit and locations each one was asked with.
func fakeGlobalping(t *testing.T, results map[string]string) (asked *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var calls []map[string]any
	targets := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			calls = append(calls, body)
			if locs, _ := json.Marshal(body["locations"]); strings.Contains(string(locs), "tags") {
				w.WriteHeader(http.StatusUnprocessableEntity) // no own probe online
				return
			}
			id := fmt.Sprintf("m%d", len(calls))
			targets[id] = body["target"].(string)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `{"id":%q}`, id)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/measurements/")
		fmt.Fprintf(w, `{"status":"finished","results":[%s]}`, results[targets[id]])
	}))
	t.Cleanup(srv.Close)
	old := globalpingAPI
	t.Cleanup(func() { globalpingAPI = old })
	globalpingAPI = srv.URL + "/v1/measurements"
	return &calls
}

// Only a probe that ran the test speaks for an address: one that failed on
// its own side leaves the address unchecked rather than closed.
func TestIranReachCountsOnlyProbesThatRan(t *testing.T) {
	t.Setenv("GLOBALPING_TAGS", "")
	fakeGlobalping(t, map[string]string{
		"192.0.2.1": `{"result":{"status":"finished","stats":{"rcv":2}}}`,
		"192.0.2.2": `{"result":{"status":"finished","stats":{"rcv":0}}}`,
		"192.0.2.3": `{"result":{"status":"failed","failureSource":"internal"}}`,
		"192.0.2.4": `{"result":{"status":"failed","failureSource":"target"}}`,
		"192.0.2.5": `{"result":{"status":"failed","failureSource":"internal"}},{"result":{"status":"finished","stats":{"rcv":1}}}`,
	})
	got := iranReach(context.Background(), []string{"192.0.2.1:443", "192.0.2.2:443", "192.0.2.3:443", "192.0.2.4:443", "192.0.2.5:443"})
	want := map[string]bool{"192.0.2.1:443": true, "192.0.2.2:443": false, "192.0.2.4:443": false, "192.0.2.5:443": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for ep, open := range want {
		if a, ok := got[ep]; !ok || a.open != open {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Each probe's handshakes are timed: the answer carries how many probes
// completed one, their mean time, the jitter (the mean difference between
// a probe's consecutive handshakes, averaged over the probes) and the
// share of handshakes lost by the probes that ran.
func TestIranReachMeasuresTheHandshakes(t *testing.T) {
	t.Setenv("GLOBALPING_TAGS", "")
	steady := `{"result":{"status":"finished","timings":[{"rtt":100},{"rtt":110},{"rtt":100},{"rtt":110}],"stats":{"total":4,"rcv":4,"avg":105}}}`
	flat := `{"result":{"status":"finished","timings":[{"rtt":200},{"rtt":200},{"rtt":200},{"rtt":200}],"stats":{"total":4,"rcv":4,"avg":200}}}`
	lossy := `{"result":{"status":"finished","timings":[{"rtt":90},{"rtt":150},{"rtt":90}],"stats":{"total":4,"rcv":3,"avg":110}}}`
	closed := `{"result":{"status":"failed","failureSource":"target"}}`
	broken := `{"result":{"status":"failed","failureSource":"internal"}}`
	fakeGlobalping(t, map[string]string{
		"192.0.2.1": steady + "," + flat,
		"192.0.2.2": steady + "," + lossy,
		"192.0.2.3": steady + "," + closed,
		"192.0.2.4": steady + "," + broken,
	})
	got := iranReach(context.Background(), []string{"192.0.2.1:443", "192.0.2.2:443", "192.0.2.3:443", "192.0.2.4:443"})
	near := func(a, b float64) bool { return a-b < 0.01 && b-a < 0.01 }
	check := func(ep string, probes int, rtt, jitter, loss float64) {
		t.Helper()
		a, ok := got[ep]
		if !ok || !a.open || a.probes != probes || !near(a.rtt, rtt) || !near(a.jitter, jitter) || !near(a.loss, loss) {
			t.Errorf("%s: %+v, want %d probes, rtt %.2f, jitter %.2f, loss %.2f", ep, a, probes, rtt, jitter, loss)
		}
	}
	check("192.0.2.1:443", 2, 152.5, 5, 0)
	check("192.0.2.2:443", 2, (420+330)/7.0, (10+60)/2.0, 100.0/8)
	// A probe that found the address closed lost every handshake it made.
	check("192.0.2.3:443", 1, 105, 10, 50)
	// One that failed on its own side made none.
	check("192.0.2.4:443", 1, 105, 10, 0)
	if a := answerOf(t, `{"result":{"status":"finished","timings":[{"rtt":80}],"stats":{"total":1,"rcv":1,"avg":80}}}`); a.jitter != -1 || a.rtt != 80 {
		t.Errorf("one handshake has no jitter: %+v", a)
	}
	// Without per-packet times, the mean comes from the counts.
	if a := answerOf(t, `{"result":{"status":"finished","stats":{"total":4,"rcv":4,"avg":70}}}`); a.rtt != 70 || a.jitter != -1 {
		t.Errorf("from the counts: %+v", a)
	}
}

// answerOf sums up a finished measurement whose results are given as JSON.
func answerOf(t *testing.T, results string) gpAnswer {
	t.Helper()
	var m gpMeasurement
	if err := json.Unmarshal([]byte(`{"status":"finished","results":[`+results+`]}`), &m); err != nil {
		t.Fatal(err)
	}
	a, err := m.answer()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The own probes go first, the volunteers' fill up to gpMinProbes in the
// same measurement, all of them inside Iran.
func TestIranReachAsksOwnProbesFirst(t *testing.T) {
	t.Setenv("GLOBALPING_TAGS", "u-test")
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"m1"}`)
			return
		}
		fmt.Fprint(w, `{"status":"finished","results":[{"result":{"status":"finished","timings":[{"rtt":50},{"rtt":52}],"stats":{"total":2,"rcv":2}}}]}`)
	}))
	defer srv.Close()
	old := globalpingAPI
	defer func() { globalpingAPI = old }()
	globalpingAPI = srv.URL + "/v1/measurements"
	if got := iranReach(context.Background(), []string{"192.0.2.9:443"}); !got["192.0.2.9:443"].open {
		t.Fatalf("got %v", got)
	}
	locs, _ := json.Marshal(body["locations"])
	if string(locs) != `[{"country":"IR","tags":["u-test"]},{"country":"IR"}]` || body["limit"] != float64(gpMinProbes) {
		t.Fatalf("asked %s with limit %v", locs, body["limit"])
	}
	opts, _ := json.Marshal(body["measurementOptions"])
	if !strings.Contains(string(opts), fmt.Sprintf(`"packets":%d`, gpPackets)) {
		t.Fatalf("options %s", opts)
	}
}

// When the API refuses the own probes' location, every address is tested
// by two of the volunteers' probes, not only the one that found it refused.
func TestIranReachFallsBackToTwoVolunteerProbes(t *testing.T) {
	t.Setenv("GLOBALPING_TAGS", "u-test")
	// More addresses than are asked about at once: the later ones start
	// after the fallback.
	results := map[string]string{}
	var eps []string
	for i := 1; i <= 20; i++ {
		ip := fmt.Sprintf("192.0.2.%d", i)
		results[ip] = `{"result":{"status":"finished","stats":{"rcv":2}}}`
		eps = append(eps, ip+":443")
	}
	calls := fakeGlobalping(t, results)
	if got := iranReach(context.Background(), eps); len(got) != len(eps) {
		t.Fatalf("got %v", got)
	}
	for _, c := range *calls {
		if locs, _ := json.Marshal(c["locations"]); !strings.Contains(string(locs), "tags") && c["limit"] != float64(2) {
			t.Fatalf("a volunteers' test asked %v probes", c["limit"])
		}
	}
}
