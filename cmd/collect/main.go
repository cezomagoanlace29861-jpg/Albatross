// Collect gathers every built-in source in one place, outside the filtered
// network, and writes a deduplicated list of servers that the apps fetch
// instead of downloading hundreds of sources themselves. It runs on a
// schedule in CI (.github/workflows/collect.yml).
//
// Output, in -out:
//
//	fresh.txt       servers from tested lists, Telegram and VPN Gate
//	hot.txt         the best of them that passed the real test
//	proven.txt      the hot servers proven healthy from inside Iran, which
//	                alone reach accounts (proven.go)
//	all-1.txt ...   everything else, best first, split into -shards files
//	meta.json       when it ran and how many servers each file holds
//	source-stats.json  what the real tests showed about each source (lanes.go)
//	tor.json        Tor's current bridges (built-in and recommended for Iran)
//
// Servers whose address does not answer at all are left out (UDP-based
// ones cannot be checked that way and are kept).
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Albatrossvpn/vp/internal/geo"
	"github.com/Albatrossvpn/vp/internal/node"
	"github.com/Albatrossvpn/vp/internal/sources"
)

// hotSize is how many servers hot.txt holds: enough for every phone to find
// a few that work on its network, small enough to fetch in a moment.
const hotSize = 300

// hotSpare is how many more candidates than hotSize the hot list starts
// with, for the ones the check from inside the country drops.
const hotSpare = 150

// newSize bounds new.txt: the latest additions to fresh.txt, which apps
// fetch often instead of downloading all of fresh.txt again.
const newSize = 4000

// minProtoTests is how many servers of each protocol the real test covers
// at least: the most shared servers are mostly VLESS, and a protocol the
// filtering spares (WireGuard works well here) was hardly ever tested.
const minProtoTests = 200

// promoteRare moves servers of protocols that have fewer than min entries
// among the first n of list forward into that range, keeping the order
// otherwise, so the real test (which takes the first n) covers them. They
// take the places of the last servers in the range of protocols that hold
// more than min there (keepReserves), so no protocol drops below min to
// make room for another; those move to just after the range.
func promoteRare(list []*entry, n, min int) {
	if n <= 0 || n >= len(list) {
		return
	}
	head := keepReserves(list[:n], list[n:], n, func(string) int { return min })
	in := make(map[*entry]bool, len(head))
	for _, e := range head {
		in[e] = true
	}
	out := head
	for _, e := range list {
		if !in[e] {
			out = append(out, e)
		}
	}
	copy(list, out)
}

// rareProtos are the protocols the world's public lists hold few servers of:
// whatever there is of them goes to fresh.txt (rareFresh), whichever source
// carried it.
var rareProtos = map[string]bool{
	"wireguard": true, "amneziawg": true, "tuic": true, "hysteria": true, "anytls": true, "naive": true,
	"juicity": true, "mieru": true, "brook": true, "wstunnel": true, "ssr": true, "snell": true,
	"openvpn": true, "ssh": true,
}

// rareFresh is how many servers of one rare protocol go to fresh.txt beyond
// those a priority source carried.
const rareFresh = 1500

// splitFresh divides the servers between fresh.txt, which every app loads
// every hour, and the long-tail lists, which an app reads one at a time (a
// phone only while nothing works). The lane picks come first; then the
// servers a priority source carries, and then those of the rare protocols,
// whatever source they came from: a protocol whose few servers sat in the
// long tail had none for most people, however many the internet held. A
// server that only dud sources carry stays out, as before.
func splitFresh(list, laneSel []*entry, inLane map[*entry]bool, maxFresh int) (fresh, rest []*entry) {
	fresh = append([]*entry(nil), laneSel...)
	taken := map[string]int{}
	rare := func(e *entry) bool {
		if !rareProtos[e.proto] || taken[e.proto] >= rareFresh {
			return false
		}
		taken[e.proto]++
		return true
	}
	for _, e := range list {
		switch {
		case inLane[e]:
		case e.dud < e.pop && len(fresh)-len(laneSel) < maxFresh && (e.fresh || rare(e)):
			fresh = append(fresh, e)
		default:
			rest = append(rest, e)
		}
	}
	return fresh, rest
}

// newList is fresh minus what the last run published, newest first, with
// the last new.txt's servers that are still listed after them.
func newList(fresh, prevFresh, prevNew []string, max int) []string {
	old := map[string]bool{}
	for _, l := range prevFresh {
		old[l] = true
	}
	was := map[string]bool{}
	for _, l := range prevNew {
		was[l] = true
	}
	var added, kept []string
	for _, l := range fresh {
		switch {
		case was[l]:
			kept = append(kept, l)
		case !old[l]:
			added = append(added, l)
		}
	}
	out := append(added, kept...)
	return out[:min(len(out), max)]
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// lastRun is what the last run published (-prev).
type lastRun struct {
	hot, fresh, new []string
}

// readLastRun reads the lists the last run published in dir: their sealed
// copies opened with keys, else the plain ones (written while -plain is
// on). Nothing for a list that is missing or that no key opens.
func readLastRun(dir string, keys []sources.ListKey) lastRun {
	read := func(name string) []string {
		if dir == "" {
			return nil
		}
		if b, err := os.ReadFile(filepath.Join(dir, name+sources.KeyedSuffix)); err == nil {
			if text := sources.OpenKeyed(keys, string(b)); text != "" {
				return strings.Fields(text)
			}
			log.Printf("prev: no list key opens %s", name+sources.KeyedSuffix)
		}
		return readLines(filepath.Join(dir, name))
	}
	return lastRun{hot: read("hot.txt"), fresh: read("fresh.txt"), new: read("new.txt")}
}

// hotCandidates is the most servers refreshHot reads from its lists, in
// their order (the last hot.txt first, then the top of fresh.txt).
const hotCandidates = 4000

// hotInput reads the candidates for refreshHot from the lists at paths, in
// order and without repeats, up to hotCandidates. A sealed list (the
// collector's .sealed files) is opened with keys; one that no key opens is
// skipped.
func hotInput(paths []string, keys []sources.ListKey) []*entry {
	var list []*entry
	seen := map[string]bool{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			log.Printf("hot: %v", err)
			continue
		}
		text := string(b)
		if _, sealed := sources.KeyedID(text); sealed {
			if text = sources.OpenKeyed(keys, text); text == "" {
				log.Printf("hot: no list key opens %s", path)
				continue
			}
		}
		for _, l := range strings.Fields(text) {
			if len(list) >= hotCandidates {
				return list
			}
			nd, err := node.ParseLink(l)
			if err != nil || seen[nd.Key()] {
				continue
			}
			seen[nd.Key()] = true
			e := &entry{link: l, proto: nd.Proto, key: node.Key64(nd.Key())}
			if !nd.UDPBased() {
				e.endpoint = nd.Endpoint()
			}
			list = append(list, e)
		}
	}
	return list
}

// writeHot writes hot.txt to out: sealed with the newest of keys as
// hot.txt.sealed, which the apps read, and plain as well when plain is set
// or there is no key to seal it with.
func writeHot(out string, links []string, keys []sources.ListKey, plain bool) error {
	text := strings.Join(links, "\n") + "\n"
	if plain || len(keys) == 0 {
		if err := os.WriteFile(filepath.Join(out, "hot.txt"), []byte(text), 0o644); err != nil {
			return err
		}
	}
	// The newest key that is not a premium one: every signed-in app opens it.
	i := 0
	for i < len(keys) && keys[i].Premium {
		i++
	}
	if i == len(keys) {
		return nil
	}
	sealed, err := sources.SealKeyed(keys[i], text)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "hot.txt"+sources.KeyedSuffix), sealed, 0o644)
}

// refreshHot re-tests the servers listed in paths (the last hot.txt and the
// top of fresh.txt, plain or sealed) and writes the ones that still work to
// hot.txt. It runs between full collections (hot.yml) and on a tester inside
// the country (deploy/tester), so the list the apps try first stays current.
func refreshHot(paths []string, out string, n int, crowd crowdStats, report, tester string, keys []sources.ListKey, plain bool) {
	list := hotInput(paths, keys)
	if len(list) == 0 {
		log.Fatal("hot: no candidates")
	}
	first, tested := realTest(list, n)
	passed := strict(confirm(first), hotSize+hotSpare)
	if report != "" {
		writeReport(report, tester, first, passed, tested)
	}
	sort.SliceStable(list, func(i, j int) bool {
		_, oki := passed[list[i]]
		_, okj := passed[list[j]]
		return oki && !okj
	})
	var ok []*entry
	for _, e := range list {
		if _, good := passed[e]; good {
			ok = append(ok, e)
		}
	}
	crowdOrder(ok, passed, crowd)
	copy(list, ok)
	hot := hotList(list, passed, hotSize+hotSpare, crowd)
	hot = fitHot(checkIran(hot, out, out).hot, hotSize)
	if len(hot) == 0 {
		log.Print("nothing passed; hot.txt left as it was")
		return
	}
	if err := writeHot(out, links(hot), keys, plain); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d hot servers", len(hot))
}

func links(es []*entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.link
	}
	return out
}

type entry struct {
	link     string
	fresh    bool
	pop      int
	endpoint string // "" for UDP-based servers
	proto    string
	key      uint64 // node.Key64 of the server's key
	// lane marks a server put forward by a source on probation or proven
	// (lanes.go); dud counts the dud sources that carry it; dead says its
	// address did not answer the probe.
	lane bool
	dud  int
	dead bool
	// speed is the slowest real download through the server among the
	// tests that passed it (bytes a second), 0 when none measured it.
	speed float64
}

type meta struct {
	Updated time.Time `json:"updated"`
	Sources int       `json:"sources"`
	Reached int       `json:"reached"`
	Found   int       `json:"found"`
	Dead    int       `json:"dead"`
	// Tested servers were connected through with the real cores; Passed
	// of them worked and lead fresh.txt, fastest first.
	Tested int            `json:"tested"`
	Passed int            `json:"passed"`
	Files  map[string]int `json:"files"`
	// Schemes counts every link scheme seen in the sources, usable or
	// not, so protocols the apps cannot run yet show up here.
	Schemes map[string]int `json:"schemes"`
	// Unsupported counts, by scheme, the links that parse but no core can
	// run yet: what we find in volume here is worth adding a core for.
	Unsupported map[string]int `json:"unsupported"`
	// Protocols counts the usable servers by protocol.
	Protocols map[string]int `json:"protocols"`
	// Channels counts the usable servers each Telegram channel posted;
	// channels that stay at zero can be dropped from the lists.
	Channels map[string]int `json:"channels"`
	// Yield is the same count for every other source, so a link that still
	// answers but carries nothing usable shows up too.
	Yield map[string]int `json:"yield"`
	// IranCheckable hot addresses can be asked about from Globalping
	// probes inside Iran (TCP, not reached through Cloudflare);
	// IranChecked of them have an answer from there younger than
	// iranFresh, and IranOpen of those answered.
	IranCheckable int `json:"iran_checkable"`
	IranChecked   int `json:"iran_checked"`
	IranOpen      int `json:"iran_open"`
	// Proven servers (proven.txt) have fresh evidence from inside Iran
	// that they are healthy (proven.go). ProvenBy names the evidence that
	// was current ("tester", "globalping", both joined with "+", or
	// "none"); ProvenFrom counts the servers each kind proved.
	Proven     int            `json:"proven"`
	ProvenBy   string         `json:"proven_by"`
	ProvenFrom map[string]int `json:"proven_from,omitempty"`
	// Lanes counts the sources per lane (lanes.go).
	Lanes map[string]int `json:"lanes,omitempty"`
}

func main() {
	out := flag.String("out", "servers", "output directory")
	keysIn := flag.String("keys", "", "the list keys handed out so far (JSON), read to keep the recent ones")
	keysOut := flag.String("keys-out", "", "where to write the list keys with this run's new one (JSON; outside -out, they are secret)")
	plain := flag.Bool("plain", false, "also write the lists unsealed, for apps from before the sealed lists")
	shards := flag.Int("shards", 8, "number of all-N.txt files")
	probe := flag.Bool("probe", true, "drop servers whose address does not answer")
	probeConc := flag.Int("probe-concurrency", 1500, "parallel reachability probes")
	probeMinutes := flag.Int("probe-minutes", 10, "stop probing addresses after this long (0: no limit); the ones not reached in time are left out of this run (the servers seen before and the most shared go first)")
	maxFresh := flag.Int("max-fresh", 60000, "most servers in fresh.txt; the rest go to the other files")
	limit := flag.Int("limit", 0, "use only the first N sources (for tests)")
	real := flag.Int("real", 8000, "connect through this many of the fresh servers with the cores in SING_BOX and XRAY; 0 skips")
	flag.Float64Var(&speedMin, "speed-min", speedMin, "bytes a second a server must carry in a real download, at each of its two tests, to pass; twice it keeps the server without question (0: no download)")
	prev := flag.String("prev", "", "directory holding the last published lists, for new.txt")
	hotFrom := flag.String("hot-from", "", "only rewrite hot.txt in -out from the servers listed in this file that pass the real test")
	crowdPath := flag.String("crowd", "", "comma-separated reports about servers from inside the country (list/servers/crowd.json, iran.json): confirmed servers first in hot.txt, failing ones left out")
	report := flag.String("report", "", "with -hot-from: write this run's results in the report form to this file (for a tester inside the country, deploy/tester)")
	tester := flag.String("tester", "", "the tester's name in -report (for example iran)")
	speedB := flag.Int("speed-bytes", speedBytes, "bytes each download of the real test carries (at least 256 KiB); a tester on a metered line passes less")
	discover := flag.String("discover", "", "find more sources (GitHub search with GITHUB_TOKEN, mentions in known lists and channels) and write sources.txt, channels.txt and discover.json to this directory; nothing else is done")
	discoverPrev := flag.String("discover-prev", "", "with -discover: the directory holding the previous run's sources.txt, channels.txt and discover.json, checked again and kept while they answer")
	discoverMinutes := flag.Int("discover-minutes", 40, "with -discover: stop looking after this many minutes and write what was found")
	extraSubs := flag.String("sources", "", "a discovered sources.txt to read beside the shipped lists")
	extraTG := flag.String("channels", "", "a discovered channels.txt to read beside the shipped lists")
	openPath := flag.String("open", "", "print the sealed list in this file opened with the keys in -keys (for the copy the build bundles in the app); nothing else is done")
	flag.Parse()
	if *speedB >= 256<<10 {
		speedBytes = *speedB
	}
	if *openPath != "" {
		text, err := openList(*openPath, readListKeys(*keysIn))
		if err != nil {
			log.Fatal(err)
		}
		os.Stdout.WriteString(text)
		return
	}
	if *discover != "" {
		runDiscover(context.Background(), *discover, *discoverPrev, os.Getenv("GITHUB_TOKEN"), *discoverMinutes)
		return
	}
	crowd := loadCrowd(*crowdPath)
	if *hotFrom != "" {
		refreshHot(strings.Split(*hotFrom, ","), *out, *real, crowd, *report, *tester, readListKeys(*keysIn), *plain)
		return
	}

	srcs := sources.Collected()
	// Discovered sources (-discover) come after the shipped ones; the
	// shipped list wins when both name a source.
	known := map[string]bool{}
	for _, s := range srcs {
		known[s.ID] = true
	}
	for _, s := range append(extraSources(*extraSubs, sources.KindSubscription), extraSources(*extraTG, sources.KindTelegram)...) {
		if !known[s.ID] {
			known[s.ID] = true
			srcs = append(srcs, s)
		}
	}
	if *limit > 0 && *limit < len(srcs) {
		srcs = srcs[:*limit]
	}
	cache, err := os.MkdirTemp("", "collect")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(cache)
	f := &sources.Fetcher{
		Direct:   &http.Client{Timeout: 90 * time.Second},
		CacheDir: cache,
	}

	var mu sync.Mutex
	byKey := map[uint64]*entry{}
	reached := 0
	schemes, channels, yield := map[string]int{}, map[string]int{}, map[string]int{}
	unsupported := map[string]int{}
	seenLinks := map[uint64]bool{}
	// The usable servers of each source, for the lanes (lanes.go).
	perSrc := make([][]*entry, len(srcs))
	ctx := context.Background()
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for i, s := range srcs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, s sources.Source) {
			defer wg.Done()
			defer func() { <-sem }()
			res := f.Fetch(ctx, s)
			if res.Err != nil || res.Text == "" {
				log.Printf("skip %s: %v", s.Label(), res.Err)
				return
			}
			fresh := s.Tier == sources.TierPriority
			var nodes []*node.Node
			if s.Kind == sources.KindVPNGate {
				for _, v := range node.ParseVPNGate(res.Text) {
					// The country travels in the name, where the app looks
					// for a hint.
					if flag := flagOf(v.Country); flag != "" && geo.Hint(v.Node.Name) == "" {
						v.Node.Name = flag + " " + v.Node.Name
						v.Node.Source = ""
					}
					nodes = append(nodes, v.Node)
				}
			} else {
				node.EachText(res.Text, func(n *node.Node) { nodes = append(nodes, n) })
			}
			// Counted on the links as the parser takes them apart, so a link
			// stuck to an escape or a word ("\\nvless://", "Germanyvmess://")
			// counts under its own scheme.
			links := node.ExtractLinks(res.Text)
			mu.Lock()
			defer mu.Unlock()
			for _, l := range links {
				h := fnv.New64a()
				h.Write([]byte(l))
				if k := h.Sum64(); !seenLinks[k] {
					seenLinks[k] = true
					scheme, _, _ := strings.Cut(l, "://")
					scheme = strings.ToLower(scheme)
					schemes[scheme]++
					// A link whose scheme we know but no core runs (e.g. a
					// protocol we have no helper for) is counted apart.
					if _, err := node.ParseLink(l); err != nil {
						unsupported[scheme]++
					}
				}
			}
			reached++
			usable := 0
			here := map[uint64]bool{}
			for _, n := range nodes {
				if n.Engine() == node.EngineNone {
					continue
				}
				usable++
				k := node.Key64(n.Key())
				e := byKey[k]
				if e == nil {
					e = &entry{link: n.Link(), proto: n.Proto, key: k}
					if !n.UDPBased() {
						e.endpoint = n.Endpoint()
					}
					byKey[k] = e
				}
				e.pop++
				e.fresh = e.fresh || fresh
				if !here[k] {
					here[k] = true
					perSrc[i] = append(perSrc[i], e)
				}
			}
			if s.Kind == sources.KindTelegram {
				channels[s.URL] = usable
			} else {
				yield[s.Label()] = usable
			}
		}(i, s)
	}
	wg.Wait()
	log.Printf("%d of %d sources reached, %d servers", reached, len(srcs), len(byKey))
	if top := topCounts(unsupported, 8); top != "" {
		// What we find but no core runs yet, most first: a protocol high
		// here is worth adding a core for (meta.json keeps the full list).
		log.Printf("found but not usable, by scheme: %s", top)
	}

	// Lanes: a source nobody has measured yet is on probation, one whose
	// servers pass is proven; both put a random sample of their servers
	// first in the probe and into a reserved share of the real test.
	ids := make([]string, len(srcs))
	for i, s := range srcs {
		ids[i] = s.ID
	}
	stats := loadStats(*prev)
	last := readLastRun(*prev, readListKeys(*keysIn))
	plan := planLanes(ids, perSrc, stats, rand.New(rand.NewSource(time.Now().UnixNano())))
	log.Printf("lanes: %d sources on probation, %d proven, %d duds, %d normal; %d servers put forward", plan.counts["probation"], plan.counts["proven"], plan.counts["dud"], plan.counts["normal"], plan.counts["marked"])

	list := make([]*entry, 0, len(byKey))
	for _, e := range byKey {
		list = append(list, e)
	}
	dead := 0
	if *probe {
		// The discovered sources bring far more servers than can be probed
		// in a run: the ones in the last published lists and the most
		// shared go first, and the budget decides how far down it gets.
		known := map[string]bool{}
		for _, list := range [][]string{last.hot, last.fresh, last.new} {
			for _, l := range list {
				if nd, err := node.ParseLink(l); err == nil && !nd.UDPBased() {
					known[nd.Endpoint()] = true
				}
			}
		}
		log.Printf("%d addresses from the last published lists go first", len(known))
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].lane != list[j].lane {
				return list[i].lane
			}
			a, b := known[list[i].endpoint], known[list[j].endpoint]
			if a != b {
				return a
			}
			return list[i].pop > list[j].pop
		})
		alive, probed := probeAll(list, *probeConc, time.Duration(*probeMinutes)*time.Minute)
		kept := list[:0]
		for _, e := range list {
			if e.endpoint == "" || alive[e.endpoint] {
				kept = append(kept, e)
			} else {
				e.dead = true
				dead++
			}
		}
		list = kept
		log.Printf("%d servers did not answer or were not reached in time (%d addresses probed), %d kept", dead, probed, len(list))
	}
	// Servers many lists carry tend to be the ones that keep working.
	sort.Slice(list, func(i, j int) bool {
		if list[i].pop != list[j].pop {
			return list[i].pop > list[j].pop
		}
		return list[i].link < list[j].link
	})
	protocols := map[string]int{}
	for _, e := range list {
		protocols[e.proto]++
	}
	// The lanes' share of the real test goes first, then the usual
	// order; a server only dud sources carry stops costing test budget.
	laneSel := plan.pick(perSrc, *real/laneShare)
	inLane := map[*entry]bool{}
	for _, e := range laneSel {
		inLane[e] = true
	}
	freshE, restE := splitFresh(list, laneSel, inLane, *maxFresh)
	log.Printf("lanes: %d servers reserved in the real test", len(laneSel))
	promoteRare(freshE, *real, minProtoTests)
	passed, tested := realTest(freshE, *real)
	passed = strict(confirm(passed), hotSize+hotSpare)
	freshE, failedE := orderFresh(freshE, passed, tested, crowd)
	restE = append(failedE, restE...)
	fresh, rest := links(freshE), links(restE)

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	m := meta{Updated: time.Now().UTC(), Sources: len(srcs), Reached: reached, Found: len(byKey), Dead: dead, Tested: len(tested), Passed: len(passed), Files: map[string]int{},
		Schemes: schemes, Unsupported: unsupported, Protocols: protocols, Channels: channels, Yield: yield,
		Lanes: plan.counts}
	// What the real tests showed about each source is kept in a file of its
	// own, for the next run (lanes.go): meta.json is read by the account
	// service on every request and stays small.
	newStats := updateStats(stats, ids, perSrc, tested, passed, time.Now())
	// The lists go out sealed with this run's key (sources.SealKeyed),
	// which the account service hands to signed-in users only; the plain
	// copies stay while apps from before the sealed lists are around.
	listKey, premiumKey, keys := newListKey(*keysIn, m.Updated)
	write := func(name string, links []string) {
		text := strings.Join(links, "\n") + "\n"
		if *plain {
			if err := os.WriteFile(filepath.Join(*out, name), []byte(text), 0o644); err != nil {
				log.Fatal(err)
			}
		}
		sealed, err := sources.SealKeyed(listKey, text)
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*out, name+sources.KeyedSuffix), sealed, 0o644); err != nil {
			log.Fatal(err)
		}
		m.Files[name] = len(links)
	}
	if *keysOut != "" {
		if err := writeListKeys(*keysOut, keys); err != nil {
			log.Fatal(err)
		}
	}
	write("fresh.txt", fresh)
	// More than hotSize candidates, so what the probes inside the country
	// cannot reach is left out and the list is still full.
	hot := hotList(freshE, passed, hotSize+hotSpare, crowd)
	ic := checkIran(hot, *prev, *out)
	m.IranCheckable, m.IranChecked, m.IranOpen = ic.checkable, ic.fresh, ic.open
	// What reaches accounts: the candidates proven healthy from inside.
	proven := decideProven(ic.hot, passed, provenEvidence{now: time.Now(), tester: crowd.tester, answers: ic.answers, cdn: ic.cdn}, hotSize)
	m.Proven, m.ProvenBy, m.ProvenFrom = len(proven.list), proven.by, proven.from
	hot = fitHot(ic.hot, hotSize)
	write("hot.txt", links(hot))
	write("proven.txt", links(proven.list))
	if *prev != "" {
		write("new.txt", newList(fresh, last.fresh, last.new, newSize))
	}
	for i := range *shards {
		lo, hi := len(rest)*i / *shards, len(rest)*(i+1) / *shards
		write(fmt.Sprintf("all-%d.txt", i+1), rest[lo:hi])
	}
	// The project's own servers (deploy/server) go out sealed, so public
	// collectors do not pick them up and overload them.
	if own, key := os.Getenv("OWN_SERVERS"), os.Getenv("OWN_KEY"); own != "" && key != "" {
		k, err := hex.DecodeString(key)
		if err != nil || len(k) != 32 {
			log.Fatal("OWN_KEY must be 64 hex digits")
		}
		// own-0.bin: the relays inside the country (OWN_RELAYS, one link
		// per line), unsharded, so every install knows them. Written even
		// when empty, so the fetch never fails.
		// An own list goes out sealed with the build key and, on top, with
		// one of this run's keys, so only a signed-in app opens it: the
		// relays' with the list key, which every plan gets (in a shutdown
		// they are the only way out), the own servers' with the premium
		// key. The plain build-key copy stays for the apps from before the
		// list keys.
		writeOwn := func(name, text string, key sources.ListKey) {
			sealed, err := sources.Seal(k, text)
			if err != nil {
				log.Fatal(err)
			}
			if *plain {
				if err := os.WriteFile(filepath.Join(*out, name), sealed, 0o644); err != nil {
					log.Fatal(err)
				}
			}
			keyed, err := sources.SealKeyed(key, string(sealed))
			if err != nil {
				log.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(*out, name+sources.KeyedSuffix), keyed, 0o644); err != nil {
				log.Fatal(err)
			}
		}
		relays := strings.Fields(os.Getenv("OWN_RELAYS"))
		writeOwn("own-0.bin", strings.Join(relays, "\n")+"\n", listKey)
		m.Files["own-0.bin"] = len(relays)
		// Split into sources.OwnBuckets parts; each app reads two. With
		// fewer servers than parts, each part gets one of them.
		links := strings.Fields(own)
		for b := range sources.OwnBuckets {
			var part []string
			for i, l := range links {
				if i%sources.OwnBuckets == b {
					part = append(part, l)
				}
			}
			if len(part) == 0 {
				part = []string{links[b%len(links)]}
			}
			name := fmt.Sprintf("own-%d.bin", b+1)
			writeOwn(name, strings.Join(part, "\n")+"\n", premiumKey)
			m.Files[name] = len(part)
		}
	}
	if br, err := torBridges(ctx, &http.Client{Timeout: 90 * time.Second}); err != nil {
		log.Printf("tor bridges: %v", err)
	} else {
		b, _ := json.Marshal(br)
		if err := os.WriteFile(filepath.Join(*out, "tor.json"), b, 0o644); err != nil {
			log.Fatal(err)
		}
		for _, v := range br {
			m.Files["tor.json"] += len(v)
		}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "meta.json"), b, 0o644); err != nil {
		log.Fatal(err)
	}
	sb, _ := json.Marshal(newStats)
	if err := os.WriteFile(filepath.Join(*out, statsFile), sb, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d fresh and %d other servers to %s", len(fresh), len(rest), *out)
}

// probeAll opens a TCP connection to every distinct address once, in
// list order, for at most budget: what it did not get to counts as not
// answering. It returns the addresses that answered and how many it tried.
func probeAll(list []*entry, conc int, budget time.Duration) (alive map[string]bool, probed int) {
	seen := map[string]bool{}
	var eps []string
	for _, e := range list {
		if e.endpoint != "" && !seen[e.endpoint] {
			seen[e.endpoint] = true
			eps = append(eps, e.endpoint)
		}
	}
	log.Printf("probing %d addresses (up to %v)", len(eps), budget)
	alive = map[string]bool{}
	var mu sync.Mutex
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	d := net.Dialer{Timeout: 3 * time.Second}
	deadline := time.Now().Add(budget)
	for _, ep := range eps {
		if budget > 0 && time.Now().After(deadline) {
			log.Printf("probe budget spent after %d of %d addresses", probed, len(eps))
			break
		}
		probed++
		wg.Add(1)
		sem <- struct{}{}
		go func(ep string) {
			defer wg.Done()
			defer func() { <-sem }()
			for range 2 {
				c, err := d.Dial("tcp", ep)
				if err == nil {
					c.Close()
					mu.Lock()
					alive[ep] = true
					mu.Unlock()
					return
				}
			}
		}(ep)
	}
	wg.Wait()
	return alive, probed
}

// flagOf turns a country code into its flag emoji.
func flagOf(code string) string {
	if len(code) != 2 {
		return ""
	}
	code = strings.ToUpper(code)
	if code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	return string([]rune{0x1F1E6 + rune(code[0]-'A'), 0x1F1E6 + rune(code[1]-'A')})
}

// topCounts formats the n largest entries of a count map, most first, as
// "key count, key count, …"; "" when the map is empty.
func topCounts(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range m {
		if v > 0 {
			all = append(all, kv{k, v})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	var b strings.Builder
	for i, e := range all {
		if i == n {
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %d", e.k, e.v)
	}
	return b.String()
}
