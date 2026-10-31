package main

import (
	"encoding/json"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// crowdStats is what was learned about servers from inside the country:
// per server key, tests passed and made. It comes from two kinds of file
// (-crowd, comma separated), both in the same form (crowdFile): the account
// service's sums of users' reports (list/servers/crowd.json), and the
// report of a tester running inside the country (deploy/tester,
// list/servers/iran.json). The collector's own test runs abroad and cannot
// see the filtering; these can, so servers confirmed from inside come first
// in hot.txt and servers that fail there are left out. With a fresh tester
// report that confirms enough servers, hot.txt holds only confirmed ones.
type crowdStats struct {
	rates  map[uint64][2]float64
	strict bool
	// demand is, per protocol, how often users chose it and found no
	// server (the service's sum of the apps' reports): the hot list keeps
	// more of what people ask for.
	demand map[string]float64
	// tester is the freshest tester report, the evidence from inside the
	// country the proven list honours (proven.go).
	tester testerReport
}

// testerReport is what a tester inside the country last reported: when,
// and the servers it confirmed (it connected through each twice, with a
// download each time, and both passed).
type testerReport struct {
	at        time.Time
	confirmed map[uint64]bool
}

// crowdFile is the file form: servers maps a server key (hex) to [passed,
// tests]; tester names the tester that wrote it (empty for users' reports);
// updated is when.
type crowdFile struct {
	Updated time.Time             `json:"updated"`
	Tester  string                `json:"tester,omitempty"`
	Days    int                   `json:"days,omitempty"`
	Servers map[string][2]float64 `json:"servers"`
	// Demand is per protocol name how often users chose it and had no
	// server, summed by the service over the same days.
	Demand map[string]float64 `json:"demand,omitempty"`
}

// testerFresh is how long a tester's report counts as current.
const testerFresh = 12 * time.Hour

// strictMin is how many servers a fresh tester report must confirm before
// hot.txt is limited to confirmed servers.
const strictMin = 100

// loadCrowd reads the files; a missing or unreadable one adds nothing.
func loadCrowd(paths string) crowdStats {
	out := crowdStats{rates: map[uint64][2]float64{}, demand: map[string]float64{}}
	for _, path := range strings.Split(paths, ",") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			log.Printf("crowd: %v", err)
			continue
		}
		var f crowdFile
		if err := json.Unmarshal(b, &f); err != nil {
			log.Printf("crowd: %s: %v", path, err)
			continue
		}
		for proto, n := range f.Demand {
			out.demand[proto] += n
		}
		confirmed := map[uint64]bool{}
		for k, v := range f.Servers {
			key, err := strconv.ParseUint(k, 16, 64)
			if err != nil || v[1] <= 0 {
				continue
			}
			r := out.rates[key]
			r[0] += v[0]
			r[1] += v[1]
			out.rates[key] = r
			if v[1] >= 2 && v[0] == v[1] {
				confirmed[key] = true
			}
		}
		if f.Tester != "" && f.Updated.After(out.tester.at) {
			out.tester = testerReport{at: f.Updated, confirmed: confirmed}
		}
		fresh := time.Since(f.Updated) < testerFresh
		if f.Tester != "" && fresh && len(confirmed) >= strictMin {
			out.strict = true
		}
		log.Printf("crowd: %s: %d servers (tester %q, %d confirmed, fresh %v)", path, len(f.Servers), f.Tester, len(confirmed), fresh)
	}
	return out
}

// demandPerSlot is how many users asking for a protocol that had no server
// add one more server of it to the hot list's reserve, up to doubling it.
const demandPerSlot = 5

// reserveOf is how many servers of proto the hot list keeps at least: the
// usual share (reserveOf), and more when users chose the protocol and had
// no server, so what people ask for is what gets kept.
func (c crowdStats) reserveOf(proto string, max int) int {
	base := reserveOf(proto, max)
	return base + min(base, int(c.demand[proto]/demandPerSlot))
}

// tier ranks a server by the tests from inside: 0 works there, 1 unknown
// or mixed, 2 fails there (enough tests, few passes).
func (c crowdStats) tier(key uint64) int {
	v, ok := c.rates[key]
	if !ok {
		return 1
	}
	ok1, n := v[0], v[1]
	switch {
	case n >= 2 && ok1/n >= 0.5:
		return 0
	case n >= 2 && ok1 == 0, n >= 5 && ok1/n < 0.25:
		return 2
	}
	return 1
}

// keep reports whether a server may be hot: never one that fails inside,
// and only confirmed ones while a tester inside confirms enough.
func (c crowdStats) keep(key uint64) bool {
	t := c.tier(key)
	return t == 0 || t == 1 && !c.strict
}

// crowdOrder sorts passed servers: those confirmed from inside first, then
// by delay with what they carried counted in (speedLead): a server that
// answers a little later and carries far more goes first.
func crowdOrder(es []*entry, passed map[*entry]time.Duration, c crowdStats) {
	score := func(e *entry) time.Duration { return passed[e] - speedLead(e.speed) }
	sort.SliceStable(es, func(i, j int) bool {
		a, b := c.tier(es[i].key), c.tier(es[j].key)
		if a != b {
			return a < b
		}
		return score(es[i]) < score(es[j])
	})
}

// speedLead is how much delay a server's measured speed makes up for.
func speedLead(bps float64) time.Duration {
	switch {
	case bps >= 4<<20:
		return 500 * time.Millisecond
	case bps >= 2<<20:
		return 350 * time.Millisecond
	case bps >= 1<<20:
		return 200 * time.Millisecond
	case bps >= 512<<10:
		return 80 * time.Millisecond
	}
	return 0
}

// writeReport writes this run's results in the crowd file form, for a
// tester running inside the country (deploy/tester): ok counts the passes
// of the first test and the confirmation, n the tests made.
func writeReport(path, tester string, first, confirmed map[*entry]time.Duration, tested map[*entry]bool) {
	f := crowdFile{Updated: time.Now().UTC(), Tester: tester, Servers: map[string][2]float64{}}
	for e := range tested {
		var ok, n float64 = 0, 1
		if _, p := first[e]; p {
			ok, n = 1, 2
			if _, c := confirmed[e]; c {
				ok = 2
			}
		}
		f.Servers[strconv.FormatUint(e.key, 16)] = [2]float64{ok, n}
	}
	b, _ := json.MarshalIndent(f, "", " ")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote the report of %d servers to %s", len(f.Servers), path)
}
