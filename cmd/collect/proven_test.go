package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// healthy is an answer from inside that proves an address: two probes,
// every handshake answered, fast and even, and open at the check before.
func healthy(now time.Time) iranSeen {
	return iranSeen{Open: true, At: now.Add(-10 * time.Minute).Unix(), Probes: 2, RTT: 120, Jitter: 8,
		Hist: []iranPast{{Open: true, At: now.Add(-150 * time.Minute).Unix()}}}
}

func provenNames(r provenResult) string {
	var out []string
	for _, e := range r.list {
		out = append(out, e.link[strings.LastIndex(e.link, "#")+1:])
	}
	return strings.Join(out, ",")
}

// Globalping's answers prove a TCP server only when every part of the bar
// holds, and the real test's download is fast enough; a server over UDP
// is never proven by them.
func TestProvenByGlobalping(t *testing.T) {
	now := time.Now()
	answers := map[string]iranSeen{}
	var cands []*entry
	add := func(name string, speed float64, a *iranSeen) *entry {
		ep := name + ".example:443"
		e := &entry{link: "vless://u@" + ep + "#" + name, endpoint: ep, speed: speed, key: uint64(len(cands) + 1)}
		if a != nil {
			answers[ep] = *a
		}
		cands = append(cands, e)
		return e
	}
	with := func(f func(*iranSeen)) *iranSeen {
		a := healthy(now)
		f(&a)
		return &a
	}
	add("good", 2<<20, with(func(*iranSeen) {}))
	add("slow-rtt", 2<<20, with(func(a *iranSeen) { a.RTT = provenMaxRTT + 1 }))
	add("jittery", 2<<20, with(func(a *iranSeen) { a.Jitter = provenMaxJitter + 1 }))
	add("unmeasured-jitter", 2<<20, with(func(a *iranSeen) { a.Jitter = -1 }))
	add("lossy", 2<<20, with(func(a *iranSeen) { a.Loss = 12.5 }))
	add("one-probe", 2<<20, with(func(a *iranSeen) { a.Probes = 1 }))
	add("was-closed", 2<<20, with(func(a *iranSeen) { a.Hist[0].Open = false }))
	add("first-check", 2<<20, with(func(a *iranSeen) { a.Hist = nil }))
	add("gap", 2<<20, with(func(a *iranSeen) { a.Hist[0].At = now.Add(-provenGap - time.Hour).Unix() }))
	add("stale", 2<<20, with(func(a *iranSeen) { a.At = now.Add(-provenFresh - time.Minute).Unix() }))
	add("closed", 2<<20, with(func(a *iranSeen) { a.Open = false }))
	add("old-record", 2<<20, &iranSeen{Open: true, At: now.Add(-time.Minute).Unix()})
	add("crawls", provenMinSpeed-1, with(func(*iranSeen) {}))
	add("unasked", 2<<20, nil)
	udp := &entry{link: "hysteria2://p@udp.example:443#udp", speed: 4 << 20, key: 99}
	cands = append(cands, udp)
	r := decideProven(cands, nil, provenEvidence{now: now, answers: answers}, hotSize)
	if got := provenNames(r); got != "good" {
		t.Fatalf("proven %q", got)
	}
	if r.by != "globalping" || r.from["globalping"] != 1 {
		t.Fatalf("by %q from %v", r.by, r.from)
	}
	// Without any fresh answer and no tester, nothing is proven.
	r = decideProven(cands, nil, provenEvidence{now: now.Add(24 * time.Hour), answers: answers}, hotSize)
	if len(r.list) != 0 || r.by != "none" {
		t.Fatalf("nothing current: %q by %q", provenNames(r), r.by)
	}
}

// A fresh report from the tester inside proves the servers it confirmed,
// over UDP too and whatever Globalping says; a stale one proves nothing.
func TestProvenByTester(t *testing.T) {
	now := time.Now()
	tcp := &entry{link: "vless://u@t.example:443#tcp", endpoint: "t.example:443", speed: 300 << 10, key: 1}
	udp := &entry{link: "hysteria2://p@u.example:443#udp", speed: 3 << 20, key: 2}
	other := &entry{link: "vless://u@o.example:443#other", endpoint: "o.example:443", speed: 3 << 20, key: 3}
	cands := []*entry{tcp, udp, other}
	rep := testerReport{at: now.Add(-time.Hour), confirmed: map[uint64]bool{1: true, 2: true}}
	r := decideProven(cands, nil, provenEvidence{now: now, tester: rep}, hotSize)
	if got := provenNames(r); got != "udp,tcp" {
		t.Fatalf("proven %q (the faster band first)", got)
	}
	if r.by != "tester" || r.from["tester"] != 2 {
		t.Fatalf("by %q from %v", r.by, r.from)
	}
	// Both vantage points current: each proves what it can.
	answers := map[string]iranSeen{"o.example:443": healthy(now)}
	r = decideProven(cands, nil, provenEvidence{now: now, tester: rep, answers: answers}, hotSize)
	if got := provenNames(r); got != "other,udp,tcp" || r.by != "tester+globalping" {
		t.Fatalf("proven %q by %q", got, r.by)
	}
	rep.at = now.Add(-provenFresh - time.Minute)
	r = decideProven(cands, nil, provenEvidence{now: now, tester: rep}, hotSize)
	if len(r.list) != 0 || r.by != "none" {
		t.Fatalf("a stale report proved %q by %q", provenNames(r), r.by)
	}
}

// A server reached through Cloudflare is proven when enough clean edge
// addresses are open from inside and it carries; its own address's
// answers do not count either way.
func TestProvenBehindCloudflare(t *testing.T) {
	now := time.Now()
	cdn := &entry{link: "vless://u@104.17.1.1:443?type=ws&security=tls&sni=c.example#cdn", endpoint: "104.17.1.1:443", speed: 1 << 20, key: 1}
	slow := &entry{link: "vless://u@104.17.1.2:443?type=ws&security=tls&sni=d.example#slow", endpoint: "104.17.1.2:443", speed: 100 << 10, key: 2}
	marks := map[*entry]bool{cdn: true, slow: true}
	answers := map[string]iranSeen{"104.17.1.1:443": {Open: false, At: now.Unix()}}
	edge := func(ip string, at time.Time) { answers[ip+":443"] = iranSeen{Open: true, At: at.Unix()} }
	edge("104.16.0.1", now.Add(-time.Hour))
	r := decideProven([]*entry{cdn, slow}, nil, provenEvidence{now: now, answers: answers, cdn: marks}, hotSize)
	if len(r.list) != 0 {
		t.Fatalf("one clean edge proved %q", provenNames(r))
	}
	edge("104.16.0.2", now.Add(-2*time.Hour))
	edge("104.16.0.3", now.Add(-provenFresh-time.Minute)) // too old to count
	r = decideProven([]*entry{cdn, slow}, nil, provenEvidence{now: now, answers: answers, cdn: marks}, hotSize)
	if got := provenNames(r); got != "cdn" || r.from["cdn"] != 1 {
		t.Fatalf("proven %q from %v", got, r.from)
	}
}

// Best first: by the band of the real test's download, then the round trip
// from inside (servers without one after), then the real test's delay;
// at most max.
func TestProvenOrder(t *testing.T) {
	now := time.Now()
	answers := map[string]iranSeen{}
	passed := map[*entry]time.Duration{}
	rep := testerReport{at: now, confirmed: map[uint64]bool{}}
	var cands []*entry
	add := func(name string, speed, rtt float64, delay time.Duration) {
		e := &entry{link: "vless://u@" + name + ":443#" + name, endpoint: name + ":443", speed: speed, key: uint64(len(cands) + 1)}
		if rtt > 0 {
			a := healthy(now)
			a.RTT = rtt
			answers[e.endpoint] = a
		} else {
			rep.confirmed[e.key] = true // proven by the tester, no round trip known
		}
		passed[e] = delay
		cands = append(cands, e)
	}
	add("mid-far", 1500<<10, 250, time.Second)
	add("mid-near", 1500<<10, 90, 2*time.Second)
	add("fast", 5<<20, 280, time.Second)
	add("mid-unknown", 1500<<10, 0, 100*time.Millisecond)
	add("base", 600<<10, 50, time.Second)
	add("mid-unknown-slower", 1500<<10, 0, 900*time.Millisecond)
	r := decideProven(cands, passed, provenEvidence{now: now, tester: rep, answers: answers}, hotSize)
	if got := provenNames(r); got != "fast,mid-near,mid-far,mid-unknown,mid-unknown-slower,base" {
		t.Fatalf("order %q", got)
	}
	if r = decideProven(cands, passed, provenEvidence{now: now, tester: rep, answers: answers}, 2); provenNames(r) != "fast,mid-near" || r.from["globalping"] != 2 || r.from["tester"] != 0 {
		t.Fatalf("cut to two: %q from %v", provenNames(r), r.from)
	}
}

// The servers reached through Cloudflare: an HTTP transport over TLS on a
// port it proxies, on one of its addresses or a name that resolves to one.
func TestCDNFronted(t *testing.T) {
	defer func(f func(context.Context, string) ([]string, error)) { lookupHost = f }(lookupHost)
	lookupHost = func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "on-cf.example":
			return []string{"2606:4700::6810:1", "104.21.5.5"}, nil
		case "elsewhere.example":
			return []string{"203.0.113.7"}, nil
		}
		return nil, fmt.Errorf("no such host")
	}
	id := "b831381d-6324-4d53-ad4f-8cda48b30811"
	mk := func(name, rest string) *entry { return &entry{link: "vless://" + id + "@" + rest + "#" + name} }
	ip := mk("ip", "104.18.2.2:443?type=ws&security=tls&sni=whatever.example&path=%2F")
	named := mk("named", "on-cf.example:443?type=grpc&security=tls&serviceName=g")
	sni := mk("sni", "198.51.100.1:2053?type=xhttp&security=tls&sni=on-cf.example&path=%2F")
	elsewhere := mk("elsewhere", "elsewhere.example:443?type=ws&security=tls&path=%2F")
	unknown := mk("unknown", "nowhere.example:443?type=ws&security=tls&path=%2F")
	plain := mk("plain", "104.18.2.2:80?type=ws&security=none&host=on-cf.example&path=%2F")
	reality := mk("reality", "104.18.2.2:443?type=grpc&security=reality&sni=on-cf.example&pbk=abc&serviceName=g")
	odd := mk("odd-port", "104.18.2.2:4443?type=ws&security=tls&sni=on-cf.example&path=%2F")
	got := cdnFronted([]*entry{ip, named, sni, elsewhere, unknown, plain, reality, odd})
	for e, want := range map[*entry]bool{ip: true, named: true, sni: true, elsewhere: false, unknown: false, plain: false, reality: false, odd: false} {
		if got[e] != want {
			t.Errorf("%s: %v, want %v", e.link[strings.LastIndex(e.link, "#")+1:], got[e], want)
		}
	}
}

// The collector keeps the freshest tester report's confirmed servers for
// the proven list; users' reports are not a tester's.
func TestCrowdKeepsTheTesterReport(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	older := filepath.Join(dir, "older.json")
	os.WriteFile(older, []byte(`{"updated":"`+at.Add(-time.Hour).Format(time.RFC3339)+`","tester":"iran","servers":{"a":[2,2]}}`), 0o644)
	iran := filepath.Join(dir, "iran.json")
	os.WriteFile(iran, []byte(`{"updated":"`+at.Format(time.RFC3339)+`","tester":"iran","servers":{"b":[2,2],"c":[1,2],"d":[0,1]}}`), 0o644)
	users := filepath.Join(dir, "crowd.json")
	os.WriteFile(users, []byte(`{"updated":"`+time.Now().UTC().Format(time.RFC3339)+`","servers":{"e":[3,3]}}`), 0o644)
	c := loadCrowd(strings.Join([]string{older, iran, users}, ","))
	if !c.tester.at.Equal(at) || len(c.tester.confirmed) != 1 || !c.tester.confirmed[0xb] {
		t.Fatalf("tester report %v %v", c.tester.at, c.tester.confirmed)
	}
}
