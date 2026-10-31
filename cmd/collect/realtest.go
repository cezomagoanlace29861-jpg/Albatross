package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Albatrossvpn/vp/internal/antiblock"
	"github.com/Albatrossvpn/vp/internal/core"
	"github.com/Albatrossvpn/vp/internal/node"
	"github.com/Albatrossvpn/vp/internal/tester"
)

// A server passes only when it also carries a real download (speedBytes)
// at speedMin or more, in each of its two tests: answering a small request
// proves little. Overloaded free servers answer and then crawl, and those
// that crawl from a data centre crawl worse for users behind Iran's
// filtering. The bar is high on purpose: the lists the apps try first hold
// only servers that carry.
//
// strict then keeps the servers that did twice as well (speedGood) and
// answered quickly (maxDelay); those between the two bars only fill the
// lists when too few cleared the higher one.
var (
	speedMin   float64 = 256 << 10
	speedBytes         = 2 << 20
)

const maxDelay = 2500 * time.Millisecond

// speedGood is the bar a server must clear to be kept without question.
func speedGood() float64 { return 2 * speedMin }

// strict keeps of what passed both tests the servers that carried at least
// speedGood and answered within maxDelay (a server that is not speed tested,
// a DNS tunnel or Tor, needs only the quick answer). When fewer than keep
// cleared that, the fastest of the rest fill up to keep, so a bad day leaves
// the lists short of nothing; the others are dropped.
func strict(passed map[*entry]time.Duration, keep int) map[*entry]time.Duration {
	if speedMin <= 0 {
		return passed
	}
	out := map[*entry]time.Duration{}
	var rest []*entry
	for e, d := range passed {
		exempt := false
		if n, err := node.ParseLink(e.link); err == nil {
			exempt = tester.SpeedExempt(n)
		}
		if d <= maxDelay && (e.speed >= speedGood() || (exempt && e.speed == 0)) {
			out[e] = d
		} else {
			rest = append(rest, e)
		}
	}
	good := len(out)
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].speed != rest[j].speed {
			return rest[i].speed > rest[j].speed
		}
		return passed[rest[i]] < passed[rest[j]]
	})
	for _, e := range rest {
		if len(out) >= keep {
			break
		}
		out[e] = passed[e]
	}
	log.Printf("strict: %d of %d servers cleared the higher bar; %d kept in all", good, len(passed), len(out))
	return out
}

// batchSeq names each test core's files apart: cores started side by side
// with one name would write one config file, and one of them start with
// the other's servers.
var batchSeq atomic.Int64

// realTest connects through up to n of list with the real cores, the way
// the app does, and reports which worked and how fast. A TCP answer alone
// says little: most such servers no longer complete a handshake, and
// phones spent minutes finding that out themselves. It needs the cores
// named by SING_BOX (and XRAY for Xray-only servers); without them it tests
// nothing.
//
// One config per address goes first: an address that is down or blocked
// fails every config on it, so testing it once leaves the budget for more
// addresses. Configs sharing an address that passed are tested with what
// is left; those sharing one that failed count as failed.
func realTest(list []*entry, n int) (passed map[*entry]time.Duration, tested map[*entry]bool) {
	sb := os.Getenv("SING_BOX")
	if sb == "" || n <= 0 {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "collect-cores")
	if err != nil {
		log.Printf("real test skipped: %v", err)
		return nil, nil
	}
	defer os.RemoveAll(dir)
	// Xray with 250 servers takes a while to come up on a busy runner;
	// given 15 seconds it was given up on, and its servers never tested.
	rt := &core.Runtime{Bin: core.Binaries{SingBox: sb, Xray: os.Getenv("XRAY")}, Dir: dir, StartWait: 60 * time.Second}
	reps, siblings := byAddress(list)
	passed = map[*entry]time.Duration{}
	tested = map[*entry]bool{}
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	run := func(es []*entry) {
		byNode := map[*node.Node]*entry{}
		var nodes []*node.Node
		for _, e := range es {
			nd, err := node.ParseLink(e.link)
			if err != nil || nd.Proto == "warpplus" || nd.Engine() == node.EngineSidecar {
				// Sidecar protocols need helpers the collector lacks:
				// they stay untested rather than failing.
				continue
			}
			byNode[nd] = e
			tested[e] = true
			nodes = append(nodes, nd)
		}
		// Several cores at once, each holding a bounded share of the servers.
		const perCore, cores = 120, 6
		sem := make(chan struct{}, cores)
		var wg sync.WaitGroup
		for lo := 0; lo < len(nodes); lo += perCore {
			part := nodes[lo:min(lo+perCore, len(nodes))]
			sem <- struct{}{}
			wg.Add(1)
			go func(part []*node.Node) {
				defer func() { <-sem; wg.Done() }()
				t := &tester.Tester{Runtime: rt, Concurrency: 64, Timeout: 8 * time.Second}
				if speedMin > 0 {
					t.SpeedBytes, t.SpeedTimeout, t.SpeedNeed, t.SpeedConcurrency = speedBytes, 12*time.Second, speedMin, 8
				}
				name := fmt.Sprintf("collect-%d", batchSeq.Add(1))
				err := t.Batch(ctx, name, part, func(r tester.Result) {
					switch {
					case r.OK && !r.Tampered && r.SpeedTested && r.Speed < speedMin:
						// Answers, but does not carry: failed.
					case r.OK && !r.Tampered:
						mu.Lock()
						e := byNode[r.Node]
						passed[e] = r.Delay
						if r.SpeedTested && (e.speed == 0 || r.Speed < e.speed) {
							e.speed = r.Speed
						}
						mu.Unlock()
					case r.Err == "the core could not start":
						// Nothing was learned about the server; it stays
						// in fresh.txt rather than counting as failed.
						mu.Lock()
						delete(tested, byNode[r.Node])
						mu.Unlock()
					}
				})
				if err != nil && ctx.Err() == nil {
					log.Printf("batch: %v", err)
				}
			}(part)
		}
		wg.Wait()
	}
	if len(reps) > n {
		reps = reps[:n]
	}
	run(reps)
	var more []*entry
	for _, r := range reps {
		if _, ok := passed[r]; ok {
			more = append(more, siblings[r]...)
		} else if tested[r] {
			for _, s := range siblings[r] {
				tested[s] = true
			}
		}
	}
	if left := n - len(reps); left > 0 {
		run(more[:min(left, len(more))])
	}
	log.Printf("real test: %d of %d addresses, %d servers in all completed a connection", countIn(reps, passed), len(reps), len(passed))
	return passed, tested
}

// byAddress returns the first config of each address, in list order, and
// the later configs on the same address. UDP servers (no TCP endpoint)
// each stand alone.
func byAddress(list []*entry) (reps []*entry, siblings map[*entry][]*entry) {
	siblings = map[*entry][]*entry{}
	first := map[string]*entry{}
	for _, e := range list {
		if e.endpoint != "" {
			if r := first[e.endpoint]; r != nil {
				siblings[r] = append(siblings[r], e)
				continue
			}
			first[e.endpoint] = e
		}
		reps = append(reps, e)
	}
	return reps, siblings
}

func countIn(es []*entry, m map[*entry]time.Duration) int {
	k := 0
	for _, e := range es {
		if _, ok := m[e]; ok {
			k++
		}
	}
	return k
}

// hotList is the fastest servers that passed, one per address, for the
// app to try before anything else.
func hotList(ordered []*entry, passed map[*entry]time.Duration, max int, crowd crowdStats) []*entry {
	var cands []*entry
	seen := map[string]bool{}
	for _, e := range ordered {
		if _, ok := passed[e]; !ok || !crowd.keep(e.key) {
			continue
		}
		if e.endpoint != "" {
			if seen[e.endpoint] {
				continue
			}
			seen[e.endpoint] = true
		}
		cands = append(cands, e)
	}
	// Half the list is the best servers in order; the other half gives
	// every protocol its share, so a user who picks any protocol finds
	// servers of it, and a protocol the filtering spares is on hand.
	out := append([]*entry(nil), cands[:min(len(cands), max/2)]...)
	queues := map[string][]*entry{}
	var protos []string
	for _, e := range cands[len(out):] {
		if queues[e.proto] == nil {
			protos = append(protos, e.proto)
		}
		queues[e.proto] = append(queues[e.proto], e)
	}
	for len(out) < max && len(protos) > 0 {
		var left []string
		for _, p := range protos {
			if len(out) >= max {
				break
			}
			q := queues[p]
			out = append(out, q[0])
			if q = q[1:]; len(q) > 0 {
				queues[p] = q
				left = append(left, p)
			}
		}
		protos = left
	}
	// Every protocol that has passing servers is in the hot list, with at
	// least its reserve of them, so a user who picks any protocol, or a
	// network that spares only one, finds servers.
	return keepReserves(out, cands, max, func(p string) int { return crowd.reserveOf(p, max) })
}

// protoReserve is how many servers of each protocol a 300-server hot list
// carries at least when that many passed; wgReserve the same for WireGuard.
const protoReserve, wgReserve = 20, 40

// reserveOf is how many servers of proto a hot list of max servers keeps at
// least: WireGuard more, as it is fast where the filtering lets it through;
// scaled to the list, so a small one (tests) keeps its shares.
func reserveOf(proto string, max int) int {
	if proto == "wireguard" {
		return min(wgReserve, max/7)
	}
	return min(protoReserve, max/15)
}

// keepReserves returns out (at most max servers, best first) holding up to
// reserve(p) servers of every protocol p in pool, taken in pool's order. A
// server added replaces the last one in out of a protocol holding more
// than its own reserve, so no protocol's share is given up for another's,
// and joins at the end.
func keepReserves(out, pool []*entry, max int, reserve func(string) int) []*entry {
	out = append([]*entry(nil), out...)
	in := map[*entry]bool{}
	count := map[string]int{}
	for _, e := range out {
		in[e] = true
		count[e.proto]++
	}
	for _, e := range pool {
		if in[e] || count[e.proto] >= reserve(e.proto) {
			continue
		}
		if len(out) >= max {
			i := len(out) - 1
			for i >= 0 && count[out[i].proto] <= reserve(out[i].proto) {
				i--
			}
			if i < 0 {
				continue
			}
			count[out[i].proto]--
			delete(in, out[i])
			out = append(out[:i], out[i+1:]...)
		}
		out = append(out, e)
		in[e] = true
		count[e.proto]++
	}
	return out
}

// fitHot cuts the hot list to max servers once the probes inside the
// country have reordered it, keeping every protocol's reserve: servers
// without an answer from inside, all the UDP ones among them, sort last,
// where a plain cut falls.
func fitHot(hot []*entry, max int) []*entry {
	return keepReserves(hot[:min(len(hot), max)], hot, max, func(p string) int { return reserveOf(p, max) })
}

// confirm tests the servers that passed once more and keeps only those that
// pass twice, download and all, with the mean of both delays: one lucky
// connection does not make a hot server. A server the second test could not
// reach (its core did not start) is tested once more; one that still has
// no second result is left out, not kept on its first.
func confirm(passed map[*entry]time.Duration) map[*entry]time.Duration {
	if len(passed) == 0 {
		return passed
	}
	again := make([]*entry, 0, len(passed))
	for e := range passed {
		again = append(again, e)
	}
	sort.Slice(again, func(i, j int) bool { return again[i].link < again[j].link })
	second, tested := realTest(again, len(again))
	if second == nil {
		return passed
	}
	var retry []*entry
	for _, e := range again {
		if !tested[e] {
			retry = append(retry, e)
		}
	}
	if len(retry) > 0 {
		third, _ := realTest(retry, len(retry))
		for e, d := range third {
			second[e] = d
		}
	}
	out := map[*entry]time.Duration{}
	for e, d := range passed {
		if d2, ok := second[e]; ok {
			out[e] = (d + d2) / 2
		}
	}
	log.Printf("confirmation: %d of %d servers passed a second time (%d needed a third try)", len(out), len(passed), len(retry))
	return out
}

// iranCheck is what checkIran learned.
type iranCheck struct {
	// hot is the hot list with what does not answer from inside left out,
	// those that do first.
	hot []*entry
	// answers is every address's record as iran-tcp.json keeps it, this
	// run's answers in.
	answers map[string]iranSeen
	// cdn marks the hot servers reached through Cloudflare (cdnFronted):
	// their own address is not asked about.
	cdn map[*entry]bool
	// checkable counts the hot servers whose address can be asked about
	// (TCP, not reached through Cloudflare); fresh how many of them have
	// an answer from inside younger than iranFresh, open how many of those
	// answered; asked the addresses asked about this run.
	checkable, fresh, open, asked int
}

// checkIran asks probes inside the country which hot addresses answer
// there, keeping the answers in iran-tcp.json (read from prevDir, written
// to outDir) so a run spends its quota on addresses not checked lately.
// It returns the hot list with what does not answer from inside left out,
// those that do first.
//
// The quota is not enough to ask about every address every run, so an
// answer is kept iranFresh and asked for again once it is iranRecheck old,
// before it runs out. What is asked goes in order: the fixed Cloudflare
// edge addresses (reach.json stands on them, and they are few), then the
// hot addresses in hot order, then edge addresses sampled at random. When
// the budget (iranChecks) or the hourly quota ends a run's asking, it is
// the end of the hot list that waits, never the top.
func checkIran(hot []*entry, prevDir, outDir string) iranCheck {
	cache := loadIranCache(prevDir)
	now := time.Now()
	r := iranCheck{cdn: cdnFronted(hot)}
	due := func(ep string) bool {
		c, ok := cache[ep]
		return !ok || now.Sub(time.Unix(c.At, 0)) >= iranRecheck
	}
	var eps []string
	queued := map[string]bool{}
	add := func(ep string) bool {
		if queued[ep] || !due(ep) {
			return false
		}
		queued[ep] = true
		eps = append(eps, ep)
		return true
	}
	// Cloudflare edge addresses too: the apps reach the account service and
	// the servers behind Cloudflare through whichever of them the network
	// lets through (reach.json).
	var fixed, sampled []string
	if edgeChecks > 0 {
		fixed, sampled = edgeCandidates()
	}
	edges := 0
	for _, ep := range fixed {
		if edges < edgeChecks && add(ep) {
			edges++
		}
	}
	budget, hotAsked := iranChecks(), 0
	for _, e := range hot {
		if e.endpoint == "" || r.cdn[e] {
			continue
		}
		if hotAsked < budget && add(e.endpoint) {
			hotAsked++
		}
	}
	for _, ep := range sampled {
		if edges < edgeChecks && add(ep) {
			edges++
		}
	}
	r.asked = len(eps)
	if len(eps) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		got := iranReach(ctx, eps)
		for ep, a := range got {
			cache[ep] = cache[ep].next(a, now)
		}
		log.Printf("globalping: %d of %d addresses asked had an answer", len(got), len(eps))
	}
	saveIranCache(outDir, cache, now)
	writeReach(outDir, cache, now)
	seen := map[string]bool{}
	for _, e := range hot {
		if e.endpoint == "" || r.cdn[e] {
			continue
		}
		r.checkable++
		if c, ok := cache[e.endpoint]; ok && now.Sub(time.Unix(c.At, 0)) < iranFresh {
			seen[e.endpoint] = c.Open
			r.fresh++
			if c.Open {
				r.open++
			}
		}
	}
	kept := hot[:0:0]
	for _, e := range hot {
		if o, ok := seen[e.endpoint]; !ok || o {
			kept = append(kept, e)
		}
	}
	r.hot = iranFirst(kept, seen)
	r.answers = cache
	log.Printf("globalping: %d of %d hot addresses that can be asked about have a fresh answer from Iran, %d of them open; %d hot addresses asked this run (at most %d), %d edge addresses; %d hot servers left out",
		r.fresh, r.checkable, r.open, hotAsked, budget, edges, len(hot)-len(kept))
	return r
}

// iranSeen is one address's last answer from inside the country, with
// the ones before it. The fields after At came with the proven list
// (proven.go); records written before them have none and prove nothing.
type iranSeen struct {
	Open bool  `json:"open"`
	At   int64 `json:"at"`
	// Probes completed handshakes; RTT is their mean time and Jitter the
	// mean difference between consecutive ones (-1: not measured), ms;
	// Loss is the handshakes without an answer, percent.
	Probes int     `json:"probes,omitempty"`
	RTT    float64 `json:"rtt,omitempty"`
	Jitter float64 `json:"jitter,omitempty"`
	Loss   float64 `json:"loss,omitempty"`
	// Hist is the answers before this one, newest first, at most
	// iranHistory of them.
	Hist []iranPast `json:"hist,omitempty"`
}

// iranPast is an earlier answer: whether the address was open, and when.
type iranPast struct {
	Open bool  `json:"open"`
	At   int64 `json:"at"`
}

// next is the record after a new answer: the answer, with the one before
// it at the head of the history.
func (s iranSeen) next(a gpAnswer, now time.Time) iranSeen {
	out := iranSeen{Open: a.open, At: now.Unix(), Probes: a.probes, RTT: tenth(a.rtt), Jitter: tenth(a.jitter), Loss: tenth(a.loss)}
	if s.At > 0 {
		out.Hist = append([]iranPast{{Open: s.Open, At: s.At}}, s.Hist...)
		out.Hist = out.Hist[:min(len(out.Hist), iranHistory)]
	}
	return out
}

// tenth rounds to a tenth, which is all the precision the file needs.
func tenth(v float64) float64 { return math.Round(v*10) / 10 }

// iranFresh is how long an answer from inside counts; iranKeep how long
// it is kept in the file at all; iranRecheck how old it may get before
// the address is asked about again, early enough that the next answer
// comes before this one runs out (runs come about every twenty minutes).
const iranFresh, iranKeep, iranRecheck = 3 * time.Hour, 24 * time.Hour, 150 * time.Minute

// iranHistory is how many earlier answers an address's record keeps.
const iranHistory = 4

const iranCacheName = "iran-tcp.json"

func loadIranCache(dir string) map[string]iranSeen {
	out := map[string]iranSeen{}
	if dir == "" {
		return out
	}
	b, err := os.ReadFile(filepath.Join(dir, iranCacheName))
	if err != nil {
		return out
	}
	json.Unmarshal(b, &out)
	return out
}

func saveIranCache(dir string, cache map[string]iranSeen, now time.Time) {
	if dir == "" {
		return
	}
	old := func(at int64) bool { return now.Sub(time.Unix(at, 0)) > iranKeep }
	for ep, c := range cache {
		if old(c.At) {
			delete(cache, ep)
			continue
		}
		n := 0
		for n < len(c.Hist) && !old(c.Hist[n].At) {
			n++
		}
		if n < len(c.Hist) {
			c.Hist = c.Hist[:n]
			cache[ep] = c
		}
	}
	b, err := json.Marshal(cache)
	if err == nil {
		os.WriteFile(filepath.Join(dir, iranCacheName), b, 0o644)
	}
}

// iranChecks bounds the hot addresses asked about from Iran per run: as
// many as the hourly quota allows at gpMinProbes tests each (Globalping
// gives 250 tests an hour without an account, 500 with GLOBALPING_TOKEN;
// the credits an own probe earns, 150 tests a day, come on top). Runs come
// about every twenty minutes, so in a busy hour the quota runs out before
// the bound: the asking stops there, and the next run goes on where the
// order (checkIran) left off.
func iranChecks() int {
	if os.Getenv("GLOBALPING_TOKEN") != "" {
		return 500 / gpMinProbes
	}
	return 250 / gpMinProbes
}

// orderFresh puts the servers that passed first, fastest first, keeps the
// untested ones after them, and moves those that failed to rest.
func orderFresh(fresh []*entry, passed map[*entry]time.Duration, tested map[*entry]bool, crowd crowdStats) (keep, failed []*entry) {
	var ok, untested []*entry
	for _, e := range fresh {
		_, good := passed[e]
		switch {
		case good:
			ok = append(ok, e)
		case tested[e]:
			failed = append(failed, e)
		default:
			untested = append(untested, e)
		}
	}
	crowdOrder(ok, passed, crowd)
	return append(ok, untested...), failed
}

// edgeAddrs are Cloudflare edge addresses tried first for reach.json:
// cloudflare.com's own and ones widely found open inside the country.
// Beside them, each run has the scan (antiblock.Scanner) pick a few real
// edges at random, so new open ranges are found over time.
var edgeAddrs = []string{"104.16.132.229", "104.16.133.229", "188.114.96.3", "188.114.97.3", "104.16.123.96", "172.64.153.1", "104.21.16.1", "104.26.0.1", "162.159.128.1", "141.101.64.1"}

// edgeChecks is how many edge addresses a run asks the probes inside about
// (a variable so tests can turn the asking off).
var edgeChecks = 30

// edgeCandidates lists edge addresses (as host:443) to check from inside:
// the fixed ones, and a random sample of real edges (checkIran asks those
// not answered lately, up to edgeChecks).
func edgeCandidates() (fixed, sampled []string) {
	seen := map[string]bool{}
	for _, ip := range edgeAddrs {
		ep := net.JoinHostPort(ip, "443")
		seen[ep] = true
		fixed = append(fixed, ep)
	}
	for _, ip := range sampleEdges() {
		if ep := net.JoinHostPort(ip, "443"); !seen[ep] {
			seen[ep] = true
			sampled = append(sampled, ep)
		}
	}
	return fixed, sampled
}

// sampleEdges scans real edges at random and returns those that answered
// a handshake (a variable so tests can do without the network).
var sampleEdges = func() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var out []string
	for _, c := range (&antiblock.Scanner{}).Scan(ctx, 120, 40) {
		out = append(out, c.IP)
	}
	return out
}

// reachFile is reach.json: the edge addresses open from inside, for the
// apps (internal/engine/reach.go).
type reachFile struct {
	Updated    time.Time `json:"updated"`
	Cloudflare []string  `json:"cloudflare"`
}

// cleanEdges is the Cloudflare edge addresses (port 443) open from inside
// the country by an answer younger than iranFresh: what reach.json lists.
func cleanEdges(cache map[string]iranSeen, now time.Time) []string {
	var open []string
	for ep, c := range cache {
		host, port, err := net.SplitHostPort(ep)
		if err != nil || port != "443" || !c.Open || now.Sub(time.Unix(c.At, 0)) > iranFresh || !antiblock.IsCloudflare(host) {
			continue
		}
		open = append(open, host)
	}
	sort.Strings(open)
	return open
}

func writeReach(dir string, cache map[string]iranSeen, now time.Time) {
	if dir == "" {
		return
	}
	open := cleanEdges(cache, now)
	b, err := json.MarshalIndent(reachFile{Updated: now.UTC(), Cloudflare: open}, "", "  ")
	if err == nil {
		os.WriteFile(filepath.Join(dir, "reach.json"), b, 0o644)
	}
	log.Printf("reach.json: %d Cloudflare edge addresses open from Iran", len(open))
}
