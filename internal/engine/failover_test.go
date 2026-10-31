//go:build unix

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Albatrossvpn/vp/internal/core"
	"github.com/Albatrossvpn/vp/internal/node"
	"github.com/Albatrossvpn/vp/internal/sources"
	"github.com/Albatrossvpn/vp/internal/tester"
)

// TestFailoverStaysInCountry connects to a group of three local servers that
// exit in the same country, then kills the one in use and later freezes the
// next one (a server that hangs instead of refusing). Each time traffic must
// move to a backup in the same group, without restarting the core and
// without changing country.
//
//	SING_BOX=... XRAY=... E2E_UPSTREAM=http://127.0.0.1:3128 E2E_HOST_IP=192.0.2.2 go test -run Failover ./internal/engine
func TestFailoverStaysInCountry(t *testing.T) {
	sb, xr, up, hostIP := os.Getenv("SING_BOX"), os.Getenv("XRAY"), os.Getenv("E2E_UPSTREAM"), os.Getenv("E2E_HOST_IP")
	if sb == "" || xr == "" || up == "" || hostIP == "" {
		t.Skip("SING_BOX, XRAY, E2E_UPSTREAM and E2E_HOST_IP must be set")
	}
	upstream := map[string]any{"type": "direct"}
	if up != "direct" {
		upURL, _ := url.Parse(up)
		upHost, upPortS, _ := net.SplitHostPort(upURL.Host)
		upPort, _ := strconv.Atoi(upPortS)
		upstream = map[string]any{"type": "http", "server": upHost, "server_port": upPort}
	}
	ports, _ := core.FreePorts(4)
	dir := t.TempDir()
	uuid := "b831381d-6324-4d53-ad4f-8cda48b30811"
	// One process per server, so each can fail on its own.
	procs := map[string]*exec.Cmd{}
	var links []string
	for i, name := range []string{"alpha", "bravo", "charlie"} {
		cfg := map[string]any{
			"log":       map[string]any{"level": "error"},
			"inbounds":  []any{map[string]any{"type": "vless", "listen": "0.0.0.0", "listen_port": ports[i], "users": []any{map[string]any{"uuid": uuid}}}},
			"outbounds": []any{upstream},
		}
		b, _ := json.Marshal(cfg)
		path := filepath.Join(dir, name+".json")
		os.WriteFile(path, b, 0o600)
		cmd := exec.Command(sb, "run", "-c", path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Signal(syscall.SIGCONT); cmd.Process.Kill(); cmd.Wait() })
		procs[name] = cmd
		waitPort(t, ports[i])
		links = append(links, fmt.Sprintf("vless://%s@%s:%d?type=tcp&security=none#%s", uuid, hostIP, ports[i], name))
	}
	sub := strings.Join(links, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sub)) }))
	defer srv.Close()

	e, err := New(Options{
		DataDir: filepath.Join(dir, "data"),
		Sources: []sources.Source{{ID: "local", Kind: sources.KindSubscription, URL: srv.URL, Tier: sources.TierPriority, Builtin: true, Enabled: true}},
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.bin = core.Binaries{SingBox: sb, Xray: xr}
	e.rt.Bin = e.bin
	e.rt.OnLine = func(c, l string) {
		if !strings.Contains(l, "INFO") {
			t.Logf("[%s] %s", c, l)
		}
	}
	s := e.Settings()
	s.ProxyPort = ports[3]
	s.Warp = false
	s.AllowPlain = true // the test servers are not encrypted
	e.settings = s
	e.Start()
	defer e.Close()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && len(e.Servers("", 0)) < 3 {
		time.Sleep(300 * time.Millisecond)
	}
	if n := len(e.Servers("", 0)); n != 3 {
		t.Fatalf("want 3 working servers, got %d (%v)", n, e.Logs())
	}
	country := e.Servers("", 0)[0].Country
	go e.Connect(country)
	waitFor(t, 30*time.Second, func() bool { return e.Snapshot().Status == StatusConnected }, e.Logs)
	e.mu.Lock()
	conn := e.conn
	e.mu.Unlock()
	pid := conn.running.SingBox.Pid()
	// The other members finish their first check in the background.
	waitFor(t, 10*time.Second, func() bool { return e.Snapshot().Conn.Ready == 2 }, e.Logs)

	proxy := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(ports[3])}
	for round, how := range []string{"killed", "frozen"} {
		before := e.Snapshot().Conn
		victim := procs[before.Server]
		if victim == nil {
			t.Fatalf("active server %q is not one of ours", before.Server)
		}
		if how == "killed" {
			victim.Process.Kill()
		} else {
			victim.Process.Signal(syscall.SIGSTOP)
		}
		start := time.Now()
		// Browse the way a user would: requests keep coming and the first
		// ones fail until the switch.
		var r tester.Result
		for time.Since(start) < 30*time.Second {
			if r = tester.Probe(t.Context(), proxy, 3*time.Second); r.OK {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		took := time.Since(start)
		after := e.Snapshot()
		if !r.OK || after.Status != StatusConnected {
			t.Fatalf("round %d: no traffic after the active server was %s (status %s: %s)", round, how, after.Status, after.Message)
		}
		if after.Conn.Server == before.Server {
			t.Fatalf("round %d: still on %s", round, before.Server)
		}
		e.mu.Lock()
		same := e.conn == conn
		e.mu.Unlock()
		if !same || conn.running.SingBox.Pid() != pid {
			t.Fatalf("round %d: the core was restarted instead of switching", round)
		}
		if after.Conn.Country != before.Country {
			t.Fatalf("round %d: country changed from %s to %s", round, before.Country, after.Conn.Country)
		}
		t.Logf("server %s: %s -> %s, traffic back after %v", how, before.Server, after.Conn.Server, took.Round(10*time.Millisecond))
		for _, l := range e.Logs() {
			if l.Time.After(start) {
				t.Logf("  +%v %s", l.Time.Sub(start).Round(10*time.Millisecond), l.Text)
			}
		}
		limit := 3 * time.Second
		if how == "frozen" {
			// A hung server is only noticed by timeouts.
			limit = 12 * time.Second
		}
		if took > limit {
			t.Fatalf("round %d: switching took %v, want under %v", round, took, limit)
		}
	}
}

// TestPickGroupStaysInCountry checks that a fastest-anywhere group keeps its
// backups in the country of the fastest server.
func TestPickGroupStaysInCountry(t *testing.T) {
	e, err := New(Options{DataDir: t.TempDir(), Sources: []sources.Source{}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.pool.Close()
	now := time.Now()
	add := func(name, country string, delay int) {
		link := fmt.Sprintf("vless://b831381d-6324-4d53-ad4f-8cda48b30811@%s.example:443?security=tls&type=tcp#%s", name, name)
		n, err := node.ParseLink(link)
		if err != nil {
			t.Fatal(err)
		}
		e.good[node.Key64(n.Key())] = &Server{Key: n.Key(), Link: link, Name: name, Country: country, Delay: delay, LastOK: now, TestedAt: now}
	}
	add("de1", "DE", 100)
	for i := 0; i < 5; i++ {
		add(fmt.Sprintf("nl%d", i), "NL", 120+i)
		add(fmt.Sprintf("de%d", i+2), "DE", 300+i)
	}
	group := e.pickGroup("", "", nil, groupSize)
	if len(group) != 6 {
		t.Fatalf("want the 6 German servers, got %d", len(group))
	}
	for _, s := range group {
		if s.Country != "DE" {
			t.Fatalf("backup %s is in %s, want DE", s.Name, s.Country)
		}
	}
	if group[0].Name != "de1" {
		t.Fatalf("group starts with %s, want the fastest", group[0].Name)
	}
	// When reconnecting, the group stays where it was even though another
	// country now has the single fastest server.
	add("fr1", "FR", 50)
	if g := e.pickGroup("", "DE", nil, groupSize); g[0].Country != "DE" {
		t.Fatalf("reconnect left the country: %s", g[0].Country)
	}
	// A country with too few servers is topped up from elsewhere.
	if g := e.pickGroup("", "FR", nil, groupSize); len(g) != groupSize || g[0].Name != "fr1" {
		t.Fatalf("want fr1 plus backups from elsewhere, got %d starting with %s", len(g), g[0].Name)
	}
}

func waitPort(t *testing.T, port int) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
			c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("port %d did not open", port)
}

func waitFor(t *testing.T, limit time.Duration, ok func() bool, logs func() []LogLine) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, l := range logs() {
		t.Logf("%s %s", l.Level, l.Text)
	}
	t.Fatal("timed out waiting")
}

func TestReachableKeepsLiveServers(t *testing.T) {
	old := tcpAnswers
	defer func() { tcpAnswers = old }()
	tcpAnswers = func(_ context.Context, addr string) bool { return strings.HasPrefix(addr, "live") }
	srv := func(name string) *Server {
		link := fmt.Sprintf("vless://b831381d-6324-4d53-ad4f-8cda48b30811@%s.example:443?security=tls&type=tcp#%s", name, name)
		return &Server{Key: name, Link: link, Name: name}
	}
	e := &Engine{ctx: context.Background()}
	list := []*Server{srv("dead"), srv("live1"), srv("live2")}
	exclude := map[string]bool{}
	got := e.reachable(list, 1, exclude)
	if len(got) != 1 || got[0].Name != "live1" {
		t.Fatalf("got %v", got)
	}
	if !exclude["dead"] || exclude["live2"] {
		t.Fatalf("exclude = %v", exclude)
	}
	// When nothing answers the device is likely offline; the list stays.
	exclude = map[string]bool{}
	if got := e.reachable(list[:1], 8, exclude); len(got) != 1 || len(exclude) != 0 {
		t.Fatalf("offline: got %v, exclude %v", got, exclude)
	}
}

func TestGroupSpreadsAcrossNetworks(t *testing.T) {
	e, err := New(Options{DataDir: t.TempDir(), Sources: []sources.Source{}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.pool.Close()
	now := time.Now()
	for i, host := range []string{"1.2.3.1", "1.2.3.2", "1.2.3.3", "5.6.7.8"} {
		link := fmt.Sprintf("vless://b831381d-6324-4d53-ad4f-8cda48b30811@%s:443?security=tls&sni=a.example&type=ws#s%d", host, i)
		n, _ := node.ParseLink(link)
		e.good[node.Key64(n.Key())] = &Server{Key: n.Key(), Link: link, Name: fmt.Sprint("s", i), Country: "DE", Delay: 100 + i, LastOK: now, TestedAt: now}
	}
	g := e.pickGroup("DE", "", nil, 3)
	if len(g) != 3 || g[0].Name != "s0" || g[2].Name != "s3" {
		t.Fatalf("group %v %v %v", g[0].Name, g[1].Name, g[2].Name)
	}
}

// The members still being tested when a race has its winner are recorded
// once they answer, even though the caller has moved on and cancelled: they
// are the backups known to work.
func TestRaceKeepsTesting(t *testing.T) {
	e, err := New(Options{DataDir: t.TempDir(), Sources: []sources.Source{}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.pool.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/proxies/slow/") {
			time.Sleep(300 * time.Millisecond)
		}
		w.Write([]byte(`{"delay": 40}`))
	}))
	defer api.Close()
	c := &connection{clashAddr: strings.TrimPrefix(api.URL, "http://")}
	fast, slow := &member{tag: "fast", server: &Server{}}, &member{tag: "slow", server: &Server{}}
	c.members = []*member{fast, slow}
	ctx, cancel := context.WithCancel(context.Background())
	if m := e.race(ctx, c, c.members, 5*time.Second); m != fast {
		t.Fatalf("the race went to %v", m)
	}
	cancel()
	time.Sleep(600 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slow.alive || slow.delay != 40 {
		t.Fatalf("the slower member was not recorded: alive %v, delay %d", slow.alive, slow.delay)
	}
}

func TestLastResortOnlyWhenNothingElse(t *testing.T) {
	e, err := New(Options{DataDir: t.TempDir(), Sources: []sources.Source{}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.pool.Close()
	now := time.Now()
	d := node.Direct()
	e.good[node.Key64(d.Key())] = &Server{Key: d.Key(), Link: d.Link(), Name: d.Name, Proto: "direct", Country: "IR", Delay: 5, LastOK: now, TestedAt: now}
	// pickGroup never hands out the serverless method itself: connectLoop
	// offers it only after fresh and recent servers have been tried.
	if g := e.pickGroup("", "", nil, groupSize); len(g) != 0 {
		t.Fatalf("with nothing else pickGroup must be empty, got %v", g)
	}
	if g := e.pickGroup("US", "", nil, groupSize); len(g) != 0 {
		t.Fatalf("a location with no server is empty too, got %v", g)
	}
	if g := e.lastResort(nil); len(g) != 1 || g[0].Proto != "direct" {
		t.Fatalf("the last resort must offer the serverless method, got %v", g)
	}
	if g := e.lastResort(map[string]bool{d.Key(): true}); len(g) != 0 {
		t.Fatalf("an excluded last resort must not come back, got %v", g)
	}
	link := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@de.example:443?security=tls&type=tcp#de"
	n, _ := node.ParseLink(link)
	e.good[node.Key64(n.Key())] = &Server{Key: n.Key(), Link: link, Name: "de", Proto: "vless", Country: "DE", Delay: 100, LastOK: now, TestedAt: now}
	for _, s := range e.pickGroup("", "", nil, groupSize) {
		if s.Proto == "direct" {
			t.Fatal("a real server must always win over the serverless method")
		}
	}
}

// TestAutomaticGroupKeepsTheEdge checks that a working edge joins an
// automatic group of another country, and stays out when a country or a
// protocol is chosen.
func TestAutomaticGroupKeepsTheEdge(t *testing.T) {
	e, err := New(Options{DataDir: t.TempDir(), Sources: []sources.Source{}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.pool.Close()
	now := time.Now()
	add := func(name, country, source string, delay int) {
		link := fmt.Sprintf("vless://b831381d-6324-4d53-ad4f-8cda48b30811@%s.example:443?security=tls&type=tcp#%s", name, name)
		n, err := node.ParseLink(link)
		if err != nil {
			t.Fatal(err)
		}
		e.good[node.Key64(n.Key())] = &Server{Key: n.Key(), Link: link, Name: name, Country: country, Source: source, Delay: delay, LastOK: now, TestedAt: now}
	}
	for i := 0; i < 8; i++ {
		add(fmt.Sprintf("in%d", i), "IN", "", 100+i)
	}
	add("edge1", "US", "edge", 250)
	hasEdge := func(g []*Server) bool {
		for _, s := range g {
			if s.Source == "edge" {
				return true
			}
		}
		return false
	}
	g := e.pickGroup("", "", nil, groupSize)
	if !hasEdge(g) || len(g) != groupSize {
		t.Fatalf("an automatic group of %d should keep the edge", len(g))
	}
	if g[0].Name != "in0" {
		t.Fatalf("the group starts with %s, want the fastest", g[0].Name)
	}
	if g := e.pickGroup("", "IN", nil, groupSize); !hasEdge(g) {
		t.Fatal("a reconnect in the same country should keep the edge too")
	}
	if hasEdge(e.pickGroup("IN", "", nil, groupSize)) {
		t.Fatal("a chosen country must not get the edge")
	}
	e.settings.Protocol = "vless"
	if hasEdge(e.pickGroup("", "", nil, groupSize)) {
		t.Fatal("a chosen protocol must not get the edge")
	}
}
