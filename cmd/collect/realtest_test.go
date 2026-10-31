package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOrderFresh(t *testing.T) {
	slow, fast, bad, later := &entry{link: "slow"}, &entry{link: "fast"}, &entry{link: "bad"}, &entry{link: "later"}
	passed := map[*entry]time.Duration{slow: 900 * time.Millisecond, fast: 200 * time.Millisecond}
	tested := map[*entry]bool{slow: true, fast: true, bad: true}
	keep, failed := orderFresh([]*entry{bad, slow, later, fast}, passed, tested, crowdStats{})
	var got []string
	for _, e := range keep {
		got = append(got, e.link)
	}
	if len(got) != 3 || got[0] != "fast" || got[1] != "slow" || got[2] != "later" {
		t.Fatalf("keep = %v", got)
	}
	if len(failed) != 1 || failed[0] != bad {
		t.Fatalf("failed = %v", failed)
	}
}

func TestByAddressAndHotList(t *testing.T) {
	a1 := &entry{link: "a1", endpoint: "1.1.1.1:443"}
	a2 := &entry{link: "a2", endpoint: "1.1.1.1:443"}
	b := &entry{link: "b", endpoint: "2.2.2.2:443"}
	u := &entry{link: "u"}
	reps, sib := byAddress([]*entry{a1, b, a2, u})
	if len(reps) != 3 || reps[0] != a1 || reps[1] != b || reps[2] != u || len(sib[a1]) != 1 || sib[a1][0] != a2 {
		t.Fatalf("reps %v siblings %v", reps, sib)
	}
	passed := map[*entry]time.Duration{a1: 1, a2: 2, u: 3}
	if got := hotList([]*entry{a1, a2, u, b}, passed, 10, crowdStats{}); len(got) != 2 || got[0] != a1 || got[1] != u {
		t.Fatalf("hot %v", got)
	}
}

func TestHotListSharesProtocols(t *testing.T) {
	// Six vless servers faster than the rest, then two of each other
	// protocol: the first half of the list is the fastest, the second half
	// gives every protocol a turn.
	var es []*entry
	passed := map[*entry]time.Duration{}
	for i := 0; i < 6; i++ {
		e := &entry{link: "v" + string(rune('0'+i)), proto: "vless", key: uint64(i + 1)}
		es = append(es, e)
		passed[e] = time.Duration(i+1) * time.Millisecond
	}
	for _, p := range []string{"hysteria2", "trojan", "shadowsocks"} {
		for i := 0; i < 2; i++ {
			e := &entry{link: p + string(rune('0'+i)), proto: p, key: uint64(100 + len(es))}
			es = append(es, e)
			passed[e] = time.Duration(100+len(es)) * time.Millisecond
		}
	}
	got := hotList(es, passed, 8, crowdStats{})
	if len(got) != 8 {
		t.Fatalf("got %d", len(got))
	}
	count := map[string]int{}
	for _, e := range got {
		count[e.proto]++
	}
	if count["vless"] != 5 || count["hysteria2"] != 1 || count["trojan"] != 1 || count["shadowsocks"] != 1 {
		t.Fatalf("shares %v", count)
	}
	// Users' reports: a server that fails for them is left out, one that
	// works for them comes first.
	crowd := crowdStats{rates: map[uint64][2]float64{1: {0, 6}, 6: {5, 5}}}
	got = hotList(es, passed, 3, crowd)
	for _, e := range got {
		if e.key == 1 {
			t.Fatal("a server failing inside stayed hot")
		}
	}
	ok := []*entry{es[0], es[1], es[5]}
	crowdOrder(ok, passed, crowd)
	if ok[0].key != 6 || ok[1].key != 2 || ok[2].key != 1 {
		t.Fatalf("order %v %v %v", ok[0].key, ok[1].key, ok[2].key)
	}
	// A fresh tester report that confirms enough servers makes hot.txt
	// hold confirmed servers only.
	crowd.strict = true
	if got = hotList(es, passed, 8, crowd); len(got) != 1 || got[0].key != 6 {
		t.Fatalf("strict: %v", got)
	}
}

func TestCrowdFiles(t *testing.T) {
	dir := t.TempDir()
	users := filepath.Join(dir, "crowd.json")
	os.WriteFile(users, []byte(`{"updated":"2026-01-01T00:00:00Z","servers":{"a":[1,3],"b":[0,2]}}`), 0o644)
	var keys []string
	for i := 0; i < strictMin; i++ {
		keys = append(keys, `"`+strconv.FormatUint(uint64(1000+i), 16)+`":[2,2]`)
	}
	iran := filepath.Join(dir, "iran.json")
	os.WriteFile(iran, []byte(`{"updated":"`+time.Now().UTC().Format(time.RFC3339)+`","tester":"iran","servers":{"a":[2,2],`+strings.Join(keys, ",")+`}}`), 0o644)
	c := loadCrowd(users + "," + iran + ",missing.json")
	if !c.strict {
		t.Fatal("a fresh tester report confirming enough servers should be strict")
	}
	if c.tier(0xa) != 0 || c.tier(0xb) != 2 || c.tier(1000) != 0 || c.tier(5) != 1 {
		t.Fatalf("tiers %d %d %d %d", c.tier(0xa), c.tier(0xb), c.tier(1000), c.tier(5))
	}
	if c.keep(5) || !c.keep(1000) {
		t.Fatal("strict keeps confirmed servers only")
	}
	// A stale tester report never makes the list strict.
	os.WriteFile(iran, []byte(`{"updated":"2026-01-01T00:00:00Z","tester":"iran","servers":{`+strings.Join(keys, ",")+`}}`), 0o644)
	if loadCrowd(iran).strict {
		t.Fatal("a stale report was taken as current")
	}
	// The report a tester writes reads back.
	e1, e2, e3 := &entry{key: 1}, &entry{key: 2}, &entry{key: 3}
	first := map[*entry]time.Duration{e1: 1, e2: 1}
	rep := filepath.Join(dir, "rep.json")
	writeReport(rep, "iran", first, map[*entry]time.Duration{e1: 1}, map[*entry]bool{e1: true, e2: true, e3: true})
	r := loadCrowd(rep)
	if r.rates[1] != [2]float64{2, 2} || r.rates[2] != [2]float64{1, 2} || r.rates[3] != [2]float64{0, 1} {
		t.Fatalf("report %v", r.rates)
	}
}

func TestNewList(t *testing.T) {
	got := newList([]string{"a", "b", "c", "d"}, []string{"a", "b", "c"}, []string{"b", "x"}, 10)
	if strings.Join(got, ",") != "d,b" {
		t.Fatalf("got %v", got)
	}
}

func TestIranFirst(t *testing.T) {
	a, b, c := &entry{link: "a", endpoint: "a:1"}, &entry{link: "b", endpoint: "b:1"}, &entry{link: "c", endpoint: "c:1"}
	got := links(iranFirst([]*entry{a, b, c}, map[string]bool{"a:1": false, "c:1": true}))
	if strings.Join(got, "") != "cba" {
		t.Fatalf("got %v", got)
	}
}

func TestDiscoverMentions(t *testing.T) {
	text := `see https://github.com/some/repo/blob/main/sub/all.txt and https://raw.githubusercontent.com/a/b/0123456789abcdef0123456789abcdef01234567/x.yaml?token=1
	join t.me/s/Good_Channel or @another_one, not t.me/joinchat/abc, https://example.com/page.html`
	subs, chans := mentions(text)
	if len(subs) != 2 || subs[0] != "https://raw.githubusercontent.com/some/repo/main/sub/all.txt" || subs[1] != "https://raw.githubusercontent.com/a/b/HEAD/x.yaml" {
		t.Fatalf("subs %v", subs)
	}
	if strings.Join(chans, ",") != "good_channel,another_one,joinchat" {
		t.Fatalf("channels %v", chans)
	}
	if canonical("ftp://x/y.txt") != "" || canonical("https://host/") != "" {
		t.Fatal("non-https or bare addresses must be dropped")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "s.txt"), []byte("# found\nhttps://a/b.txt\n\nhttps://c/d.txt\n"), 0o644)
	got := extraSources(filepath.Join(dir, "s.txt"), "sub")
	if len(got) != 2 || got[1].URL != "https://c/d.txt" || got[0].Tier != 1 {
		t.Fatalf("extra %v", got)
	}
}

// The check from inside the country keeps its answers between runs: with
// fresh answers for every address nothing is asked, what does not answer
// inside is left out, and what does comes first.
func TestCheckIranUsesItsCache(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()
	cache := map[string]iranSeen{
		"a:1": {Open: false, At: now - 60},
		"b:1": {Open: true, At: now - 60},
		"d:1": {Open: true, At: now - int64(iranKeep/time.Second) - 1}, // too old: dropped from the file
	}
	b, _ := json.Marshal(cache)
	os.WriteFile(filepath.Join(dir, iranCacheName), b, 0o644)
	a := &entry{link: "vless://a@a:1#A", endpoint: "a:1"}
	bb := &entry{link: "vless://b@b:1#B", endpoint: "b:1"}
	c := &entry{link: "hysteria2://c@c:1#C"} // no TCP endpoint: never checked, kept
	// Nothing left to ask, and no edge addresses either: no probe is used.
	defer func(n int) { edgeChecks = n }(edgeChecks)
	edgeChecks = 0
	r := checkIran([]*entry{a, bb, c}, dir, dir)
	if got := links(r.hot); len(got) != 2 || got[0] != bb.link || got[1] != c.link {
		t.Fatalf("kept %v", got)
	}
	if r.checkable != 2 || r.fresh != 2 || r.open != 1 || r.asked != 0 {
		t.Fatalf("checkable %d fresh %d open %d asked %d", r.checkable, r.fresh, r.open, r.asked)
	}
	saved := loadIranCache(dir)
	if _, ok := saved["d:1"]; ok || len(saved) != 2 {
		t.Fatalf("saved %v", saved)
	}
}

// The quota is short: the fixed edge addresses are asked first, then the
// hot addresses in hot order up to the budget, then the sampled edges. An
// answer is asked for again before it runs out (iranRecheck), and the one
// before it moves into the history. A server reached through Cloudflare is
// not asked about, and not left out for what its own address did.
func TestCheckIranAsksInOrder(t *testing.T) {
	t.Setenv("GLOBALPING_TAGS", "")
	t.Setenv("GLOBALPING_TOKEN", "")
	var mu sync.Mutex
	asked := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			asked[body["target"].(string)] = true
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `{"id":%q}`, body["target"])
			return
		}
		fmt.Fprint(w, `{"status":"finished","results":[`+strings.Repeat(`{"result":{"status":"finished","timings":[{"rtt":90},{"rtt":95},{"rtt":90},{"rtt":95}],"stats":{"total":4,"rcv":4}}},`, gpMinProbes-1)+
			`{"result":{"status":"finished","timings":[{"rtt":90},{"rtt":95},{"rtt":90},{"rtt":95}],"stats":{"total":4,"rcv":4}}}]}`)
	}))
	defer srv.Close()
	defer func(api string, poll time.Duration, n int, fixed []string, sample func() []string, lookup func(context.Context, string) ([]string, error)) {
		globalpingAPI, gpPoll, edgeChecks, edgeAddrs, sampleEdges, lookupHost = api, poll, n, fixed, sample, lookup
	}(globalpingAPI, gpPoll, edgeChecks, edgeAddrs, sampleEdges, lookupHost)
	globalpingAPI, gpPoll = srv.URL+"/v1/measurements", time.Millisecond
	edgeChecks, edgeAddrs = 3, []string{"104.16.0.1", "104.16.0.2"}
	sampleEdges = func() []string { return []string{"104.16.0.1", "104.16.9.9", "104.16.9.10"} }
	lookupHost = func(context.Context, string) ([]string, error) { return nil, fmt.Errorf("no lookups here") }

	dir := t.TempDir()
	now := time.Now()
	var hot []*entry
	for i := 0; i < iranChecks()+5; i++ {
		host := fmt.Sprintf("10.1.%d.%d", i/250, i%250)
		hot = append(hot, &entry{link: "vless://b831381d-6324-4d53-ad4f-8cda48b30811@" + host + ":443?type=tcp#h", endpoint: host + ":443"})
	}
	// Answered a minute ago: not asked again. Answered past iranRecheck:
	// asked again, the old answer kept as history.
	cache := map[string]iranSeen{
		hot[0].endpoint: {Open: true, At: now.Add(-time.Minute).Unix()},
		hot[1].endpoint: {Open: true, At: now.Add(-iranRecheck - time.Minute).Unix(), Probes: 2},
	}
	cdn := &entry{link: "vless://b831381d-6324-4d53-ad4f-8cda48b30811@104.17.1.1:443?type=ws&security=tls&host=cdn.example.com&sni=cdn.example.com&path=%2F#c", endpoint: "104.17.1.1:443"}
	cache[cdn.endpoint] = iranSeen{Open: false, At: now.Add(-time.Minute).Unix()}
	hot = append([]*entry{cdn}, hot...)
	b, _ := json.Marshal(cache)
	os.WriteFile(filepath.Join(dir, iranCacheName), b, 0o644)

	r := checkIran(hot, dir, dir)
	if asked["104.17.1.1"] || !r.cdn[cdn] {
		t.Fatalf("the server behind Cloudflare was asked about (marked %v)", r.cdn[cdn])
	}
	kept := false
	for _, e := range r.hot {
		kept = kept || e == cdn
	}
	if !kept {
		t.Fatal("the server behind Cloudflare was left out for its own address's answer")
	}
	if asked["10.1.0.0"] || !asked["10.1.0.1"] {
		t.Fatal("a fresh answer was asked for again, or a nearly spent one was not")
	}
	budget := iranChecks()
	for i, e := range hot[1:] {
		host := e.endpoint[:len(e.endpoint)-4]
		// hot[1:][0] had a fresh answer, so the budget reaches one further.
		if want := i >= 1 && i <= budget; asked[host] != want {
			t.Fatalf("hot address %d asked %v, want %v", i, asked[host], want)
		}
	}
	for _, edge := range []string{"104.16.0.1", "104.16.0.2", "104.16.9.9"} {
		if !asked[edge] {
			t.Fatalf("edge %s not asked (%d asked)", edge, len(asked))
		}
	}
	if asked["104.16.9.10"] {
		t.Fatal("more edge addresses asked than edgeChecks")
	}
	if r.asked != budget+3 || r.checkable != len(hot)-1 || r.fresh != budget+1 {
		t.Fatalf("asked %d, checkable %d, fresh %d", r.asked, r.checkable, r.fresh)
	}
	again := r.answers[hot[2].endpoint]
	if !again.Open || again.Probes != gpMinProbes || again.RTT != 92.5 || again.Jitter != 5 || len(again.Hist) != 1 || !again.Hist[0].Open {
		t.Fatalf("re-asked address: %+v", again)
	}
	// The cache on disk has the new answers and the history.
	if saved := loadIranCache(dir); len(saved[hot[2].endpoint].Hist) != 1 {
		t.Fatalf("saved %+v", saved[hot[2].endpoint])
	}
}

// A record keeps its last iranHistory answers before the latest, and none
// older than iranKeep.
func TestIranHistory(t *testing.T) {
	now := time.Now()
	var s iranSeen
	for i := 0; i < iranHistory+3; i++ {
		s = s.next(gpAnswer{open: i%2 == 0, probes: 2, rtt: 100.04, jitter: -1}, now.Add(time.Duration(i)*time.Hour))
	}
	if len(s.Hist) != iranHistory || s.Hist[0].At != now.Add(time.Duration(iranHistory+1)*time.Hour).Unix() || s.RTT != 100 || s.Jitter != -1 {
		t.Fatalf("%+v", s)
	}
	dir := t.TempDir()
	cache := map[string]iranSeen{"x:1": {Open: true, At: now.Unix(), Hist: []iranPast{{true, now.Add(-time.Hour).Unix()}, {true, now.Add(-iranKeep - time.Hour).Unix()}}}}
	saveIranCache(dir, cache, now)
	if got := loadIranCache(dir)["x:1"]; len(got.Hist) != 1 {
		t.Fatalf("kept %+v", got)
	}
}

// Servers of a protocol the most shared lists lack are still among the
// ones the real test covers, and the hot list keeps room for WireGuard.
func TestRareProtocolsAreTestedAndKept(t *testing.T) {
	var list []*entry
	for i := 0; i < 50; i++ {
		list = append(list, &entry{link: "vless://x@v" + string(rune('a'+i%26)) + ":1#" + string(rune('A'+i%26)), proto: "vless", pop: 100 - i})
	}
	for i := 0; i < 5; i++ {
		list = append(list, &entry{link: "wireguard://k@w:" + string(rune('1'+i)) + "#W", proto: "wireguard", pop: 1})
	}
	promoteRare(list, 20, 3)
	wg := 0
	for _, e := range list[:20] {
		if e.proto == "wireguard" {
			wg++
		}
	}
	if wg != 3 || len(list) != 55 {
		t.Fatalf("%d wireguard among the first 20, %d entries", wg, len(list))
	}
	if list[0].proto != "vless" || list[0].pop != 100 {
		t.Fatalf("the most shared server moved: %+v", list[0])
	}

	out := list[:4]
	cands := list
	got := keepReserves(out, cands, 4, func(p string) int {
		if p == "wireguard" {
			return 2
		}
		return 0
	})
	wg = 0
	for _, e := range got {
		if e.proto == "wireguard" {
			wg++
		}
	}
	if len(got) != 4 || wg != 2 {
		t.Fatalf("reserve: %d entries, %d wireguard", len(got), wg)
	}
	if got[0] != list[0] {
		t.Fatal("the first server was replaced instead of the last")
	}
}

// Every protocol with passing servers has at least protoReserve in the
// hot list, even when the best half is all one protocol.
// Users who chose a protocol and found no server raise its reserve in the
// hot list, up to doubling it; nothing asked for leaves it as it was.
func TestDemandRaisesReserve(t *testing.T) {
	base := reserveOf("tuic", 300)
	if got := (crowdStats{}).reserveOf("tuic", 300); got != base {
		t.Fatalf("no demand: %d, want %d", got, base)
	}
	c := crowdStats{demand: map[string]float64{"tuic": 10 * demandPerSlot}}
	if got := c.reserveOf("tuic", 300); got != base+10 {
		t.Fatalf("ten slots of demand: %d, want %d", got, base+10)
	}
	c.demand["tuic"] = 1000 * demandPerSlot
	if got := c.reserveOf("tuic", 300); got != 2*base {
		t.Fatalf("huge demand: %d, want the double, %d", got, 2*base)
	}
}

func TestHotListHasEveryProtocol(t *testing.T) {
	var ordered []*entry
	passed := map[*entry]time.Duration{}
	add := func(proto string, n int, d time.Duration) {
		for i := 0; i < n; i++ {
			e := &entry{link: proto + "://x@" + proto + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ":1#X", proto: proto, endpoint: proto + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ":1"}
			ordered = append(ordered, e)
			passed[e] = d
		}
	}
	add("vless", 400, time.Millisecond)
	add("trojan", 30, time.Second)
	add("ss", 25, time.Second)
	add("wireguard", 50, time.Second)
	add("tuic", 3, time.Second)
	hot := hotList(ordered, passed, 300, crowdStats{rates: map[uint64][2]float64{}})
	if len(hot) != 300 {
		t.Fatalf("%d hot", len(hot))
	}
	count := map[string]int{}
	for _, e := range hot {
		count[e.proto]++
	}
	if count["trojan"] < protoReserve || count["ss"] < protoReserve || count["wireguard"] < wgReserve || count["tuic"] != 3 {
		t.Fatalf("protocol counts: %v", count)
	}
}

// Every protocol keeps its reserve when several need room at once, and
// still after the probes inside the country put the addresses that answer
// first and the list is cut to hotSize: UDP servers have no answer there
// and sort last, where the cut falls.
func TestHotListReservesSurviveTheCut(t *testing.T) {
	var ordered []*entry
	passed := map[*entry]time.Duration{}
	seen := map[string]bool{}
	add := func(proto string, n int, udp bool) {
		for i := 0; i < n; i++ {
			host := fmt.Sprintf("%s%d", proto, i)
			e := &entry{link: proto + "://x@" + host + ":1#X", proto: proto}
			if !udp {
				e.endpoint = host + ":1"
				seen[e.endpoint] = true // answers from inside
			}
			ordered = append(ordered, e)
			passed[e] = time.Second
		}
	}
	add("vless", 600, false)
	protos := []string{"trojan", "ss", "vmess", "hysteria2", "tuic", "anytls", "ssh", "socks", "http", "naive", "mieru", "wireguard"}
	for _, p := range protos {
		udp := p == "hysteria2" || p == "tuic" || p == "wireguard"
		n := 30
		if p == "wireguard" {
			n = 60
		}
		add(p, n, udp)
	}
	hot := hotList(ordered, passed, hotSize+hotSpare, crowdStats{rates: map[uint64][2]float64{}})
	check := func(stage string, hot []*entry, max int) {
		t.Helper()
		count := map[string]int{}
		for _, e := range hot {
			count[e.proto]++
		}
		for _, p := range protos {
			if count[p] < reserveOf(p, max) {
				t.Errorf("%s: %d %s servers, want %d (%v)", stage, count[p], p, reserveOf(p, max), count)
			}
		}
		if len(hot) > max {
			t.Errorf("%s: %d servers, want at most %d", stage, len(hot), max)
		}
	}
	check("hot list", hot, hotSize+hotSpare)
	hot = fitHot(iranFirst(hot, seen), hotSize)
	check("after the cut", hot, hotSize)
	if len(hot) != hotSize {
		t.Fatalf("%d servers after the cut", len(hot))
	}
}

// Bringing a rare protocol forward never pushes another protocol's
// servers out of the tested range below their own minimum.
func TestPromoteRareKeepsOtherMinimums(t *testing.T) {
	var list []*entry
	add := func(proto string, n int) {
		for i := 0; i < n; i++ {
			list = append(list, &entry{link: fmt.Sprintf("%s://x@h%d:1", proto, len(list)), proto: proto})
		}
	}
	add("vless", 17)
	add("wireguard", 3) // at the end of the tested range, exactly at the minimum
	add("trojan", 2)
	add("vless", 10)
	promoteRare(list, 20, 3)
	count := map[string]int{}
	for _, e := range list[:20] {
		count[e.proto]++
	}
	if count["wireguard"] != 3 || count["trojan"] != 2 || count["vless"] != 15 || len(list) != 32 {
		t.Fatalf("first 20: %v (%d entries)", count, len(list))
	}
	seen := map[*entry]bool{}
	for _, e := range list {
		if seen[e] {
			t.Fatal("an entry is listed twice")
		}
		seen[e] = true
	}
}

// A probe budget of zero is no limit, not "probe nothing".
func TestProbeAllWithoutBudget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	e := &entry{link: "x", endpoint: ln.Addr().String()}
	alive, probed := probeAll([]*entry{e}, 4, 0)
	if probed != 1 || !alive[e.endpoint] {
		t.Fatalf("probed %d, alive %v", probed, alive)
	}
}

// Servers that carried twice the bar and answered quickly are kept; those
// between the bars fill up only what is short, fastest first; a slow
// answer counts against a server however fast its download.
func TestStrictKeepsTheBest(t *testing.T) {
	old := speedMin
	speedMin = 256 << 10
	defer func() { speedMin = old }()
	mk := func(i int, speed float64) *entry {
		return &entry{link: fmt.Sprintf("vless://b831381d-6324-4d53-ad4f-8cda48b30811@10.0.0.%d:443?type=tcp#s%d", i, i), speed: speed}
	}
	fast, fast2, middling, slowish, lagging := mk(1, 2<<20), mk(2, 600<<10), mk(3, 400<<10), mk(4, 300<<10), mk(5, 5<<20)
	passed := map[*entry]time.Duration{fast: 300 * time.Millisecond, fast2: time.Second, middling: 500 * time.Millisecond, slowish: 400 * time.Millisecond, lagging: 3 * time.Second}
	got := strict(passed, 2)
	if len(got) != 2 || got[fast] == 0 || got[fast2] == 0 {
		t.Fatalf("enough cleared the bar: %v", names(got))
	}
	got = strict(passed, 4)
	if len(got) != 4 || got[middling] == 0 || got[lagging] == 0 || got[slowish] != 0 {
		t.Fatalf("filled with the fastest of the rest: %v", names(got))
	}
	// No download bar (-speed-min 0): everything that passed stays.
	speedMin = 0
	if got := strict(passed, 1); len(got) != len(passed) {
		t.Fatalf("without a bar: %v", names(got))
	}
}

func names(m map[*entry]time.Duration) []string {
	var out []string
	for e := range m {
		out = append(out, e.link[strings.LastIndex(e.link, "#")+1:])
	}
	sort.Strings(out)
	return out
}

func TestTopCounts(t *testing.T) {
	got := topCounts(map[string]int{"ssh": 100, "snell": 50, "warp": 0, "wstunnel": 200}, 2)
	if got != "wstunnel 200, ssh 100" {
		t.Fatalf("topCounts: %q", got)
	}
	if topCounts(map[string]int{}, 5) != "" {
		t.Fatal("empty should be blank")
	}
}
