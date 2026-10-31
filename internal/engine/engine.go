// Package engine finds working servers continuously and keeps the user
// connected to the fastest ones in the country they picked.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Albatrossvpn/vp/internal/account"
	"github.com/Albatrossvpn/vp/internal/antiblock"
	"github.com/Albatrossvpn/vp/internal/core"
	"github.com/Albatrossvpn/vp/internal/geo"
	"github.com/Albatrossvpn/vp/internal/node"
	"github.com/Albatrossvpn/vp/internal/pool"
	"github.com/Albatrossvpn/vp/internal/sources"
	"github.com/Albatrossvpn/vp/internal/tester"
)

// Options configure a new engine.
type Options struct {
	DataDir  string // settings, caches and logs
	AppDirs  []string
	ExeName  string // this app's executable name, routed direct in TUN mode
	IsAdmin  func() bool
	SysProxy SystemProxy
	Version  string
	// Autostart turns launching at login on or off.
	Autostart func(enabled bool) error
	// Sources replaces the built-in source list (tests use it).
	Sources []sources.Source
	// LocalDNS is a resolver address the cores use for direct traffic and
	// server names on systems whose resolver they cannot ask themselves
	// (Android). Empty means the system's. SetLocalDNS changes it.
	LocalDNS string
	// NetworkDNS, when set, returns the network's resolver as it is now,
	// for DNS tunnels where the cores ask the system's resolver (Windows):
	// a tunnel needs an address to send its queries to, and the network's
	// own is the one that keeps answering when foreign ones are cut. It is
	// asked each time cores start; LocalDNS, when set, comes first.
	NetworkDNS func() string
	// AccountURLs replaces the build's account service addresses (tests use
	// it); nil means AccountURL.
	AccountURLs []string
	// Mobile tunes defaults and pacing for phones: lighter scanning, no
	// large lists by default, and "proxy" mode is the device-wide VPN.
	Mobile bool
	// InstallUpdate opens a downloaded installer of a newer version (see
	// GetUpdate); nil means the download page is opened instead.
	InstallUpdate func(path string) error
	// LANAddrs lists this device's addresses on its hotspot and Wi-Fi, for
	// systems where the engine cannot list them itself (Android 11 and
	// later refuse it); nil means the engine asks the system.
	LANAddrs func() []string
	// Badge is told the plan of the signed-in account ("bronze", "silver",
	// "gold", "vip") whenever it changes, and "" when nobody is signed in,
	// so the app can mark its icon with it. Nil means no badge.
	Badge func(tier string)
}

// SystemProxy sets and clears the OS proxy settings.
type SystemProxy interface {
	Set(addr string, bypass []string) error
	Clear() error
	// Owned reports whether the OS proxy currently points at addr, which
	// means a previous run crashed without cleaning up.
	Owned(addr string) bool
}

// Engine is the app's brain. All exported methods are safe to call from
// any goroutine.
type Engine struct {
	opt  Options
	dir  string
	bin  core.Binaries
	rt   *core.Runtime
	pool *pool.Pool

	acct *account.Client // nil when this build has no accounts
	// speed is the in-app speed test (speedtest.go).
	speed speedRun
	// crowdURLs are the account service's addresses, which also collect
	// crowd stats (crowd.go); crowdAt is when tests were last reported.
	crowdURLs []string
	crowdAt   time.Time
	// badge is the plan last given to opt.Badge, badged whether any was.
	badge  string
	badged bool
	// planned says applyPlan has run; planOwn and planEdge whether the plan
	// had the own servers and the edge then.
	planned, planOwn, planEdge bool
	// loginStep is what signing in is doing now, for the UI ("" when not).
	loginStep string
	// storeAsked is when the store was last asked for without an account
	// (unix seconds), so the sign-in screen can offer buying one.
	storeAsked atomic.Int64
	// httpsOK is when a server last passed a test with a genuine
	// certificate, which shows this device checks certificates correctly
	// (see distrust).
	httpsOK time.Time
	fetcher *sources.Fetcher
	// privateDirect makes direct calls to the account service (ech.go);
	// reach knows the ways to it (reach.go), reachAt when its hints from
	// the collector were last fetched.
	privateDirect *http.Client
	warmPrivate   func(host string)
	reach         *reachStore
	reachAt       time.Time
	tester        *tester.Tester
	resolver      *antiblock.Resolver
	cf            *antiblock.Scanner

	mu           sync.Mutex
	settings     Settings
	srcs         []sources.Source
	srcStats     []SourceStat
	srcStatsByID map[string]SourceStat
	srcIndex     map[string]int
	good         map[uint64]*Server
	acctSlice    acctSlice // the account's slice of the servers (slice.go)
	net          netLearn  // which kinds of server pass on this network
	conn         *connection
	proxyMu      sync.Mutex // serializes setProxy and clearProxy
	proxyOn      bool       // guarded by proxyMu
	status       string
	message      string
	target       string    // country the user wants ("" = fastest anywhere)
	wantConnect  bool      // keep trying to connect until a server is found
	updating     bool      // GetUpdate is downloading
	forcing      bool      // forceUpdate's retry loop is running
	updateAt     time.Time // when the manifest was last fetched (checkUpdateIf)
	checking     bool      // a fetch of the manifest is running
	// badDownloads counts installers of a version that did not match
	// their checksum, so a broken file is not fetched again and again.
	badDownloads map[string]int
	connGen      int // bumped by every user connect/disconnect; stale loops exit
	// startMu lets one connection attempt run at a time: two at once each
	// start a core on the proxy port and the second finds it taken.
	startMu     sync.Mutex
	attempt     context.CancelFunc // cancels the attempt in progress; under mu
	poolStats   [4]int             // total, untested, dead, ok; refreshed by statsLoop
	protoStats  map[string][2]int  // by protocol: servers not known dead, and those that worked
	protoFailed map[string]int     // by protocol: servers tried that failed
	scan        scanState
	logs        []LogLine
	localDNS    string
	reg         regimeState   // the network's filtering regime (regime.go)
	regNet      string        // the network reg was probed on
	regAsked    time.Time     // when a regime probe was last asked for on demand
	regProbing  atomic.Bool   // a regime probe is running
	irRanges    []ipRange     // the country's address ranges (geoip-ir.srs), for domesticEndpoint
	notice      string        // a message for the user that outlives status changes
	install     uint32        // this installation's random number (installNumber)
	helper      *core.Process // the "Help others" Snowflake proxy (help.go)
	update      *UpdateInfo   // a newer version, from update.json
	uiSeen      time.Time     // last time the UI asked for a snapshot
	countries   []CountryInfo // cached for countryCache
	countriesAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	refill chan struct{}
	wg     sync.WaitGroup
}

// LogLine is one entry of the activity log shown in the UI.
type LogLine struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

// Status values.
const (
	StatusIdle         = "idle"
	StatusSearching    = "searching"
	StatusConnecting   = "connecting"
	StatusConnected    = "connected"
	StatusReconnecting = "reconnecting"
)

// New prepares an engine; Start begins work.
func New(opt Options) (*Engine, error) {
	if err := os.MkdirAll(opt.DataDir, 0o755); err != nil {
		return nil, err
	}
	for _, d := range []string{"cache", "run", "rules"} {
		os.MkdirAll(filepath.Join(opt.DataDir, d), 0o755)
	}
	freshStart(opt.DataDir)
	if opt.IsAdmin == nil {
		opt.IsAdmin = func() bool { return false }
	}
	if opt.SysProxy == nil {
		opt.SysProxy = noProxy{}
	}
	e := &Engine{
		opt:          opt,
		dir:          opt.DataDir,
		good:         map[uint64]*Server{},
		srcStatsByID: map[string]SourceStat{},
		status:       StatusIdle,
		wake:         make(chan struct{}, 1),
		refill:       make(chan struct{}, 1),
		localDNS:     opt.LocalDNS,
	}
	e.settings = loadSettings(e.dir, opt.Mobile)
	e.install = installNumber(e.dir)
	e.target = e.settings.Country
	e.bin = core.Locate(append(opt.AppDirs, e.dir)...)
	e.rt = &core.Runtime{Bin: e.bin, Dir: filepath.Join(e.dir, "run"), TorBridges: e.torBridges}
	p, err := pool.Open(filepath.Join(e.dir, "pool.dat"))
	if err != nil {
		return nil, err
	}
	e.pool = p
	direct := &http.Client{Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}}
	e.reach = newReachStore(e.dir, func() []string {
		var out []string
		if e.cf != nil {
			for _, c := range e.cf.Best() {
				out = append(out, c.IP)
			}
		}
		return out
	})
	e.privateDirect, e.warmPrivate = newPrivateDirect(direct, e.reach)
	e.fetcher = &sources.Fetcher{Direct: direct, Proxy: e.proxyClient, Private: e.privateDirect, CacheDir: filepath.Join(e.dir, "cache")}
	if urls := accountURLs(opt); len(urls) > 0 {
		e.acct = account.New(urls, account.SessionPath(e.dir), e.accountHTTP)
		e.acct.Version = opt.Version
		e.acct.Device, e.acct.DeviceName = account.DeviceID(e.dir), deviceName(opt)
		e.crowdURLs = urls
	}
	e.tester = &tester.Tester{Runtime: e.rt}
	e.resolver = &antiblock.Resolver{Client: e.anyClient, DoH: func() bool { return e.fetchMode() == sources.ModeNormal || e.proxyClient() != nil }}
	e.cf = &antiblock.Scanner{}
	e.applyIntensity()
	if err := installRules(filepath.Join(e.dir, "rules")); err != nil {
		e.logf("warn", "Could not install the local-site lists: %v", err)
	}
	e.irRanges, _ = readIPRuleSet(filepath.Join(e.dir, "rules", "geoip-ir.srs"))
	e.fetcher.Mode = e.fetchMode
	e.fetcher.Warp = e.warpLinks
	e.fetcher.Account = e.sliceText
	sources.ExtraMirrors = e.domesticMirrors
	e.loadServers()
	e.net.load(e.dir)
	e.loadRegime()
	e.net.use(opt.LocalDNS)
	e.loadSourceStats()
	e.buildSources()
	return e, nil
}

// serverGen is the generation of the server results kept on disk. A
// version that judges servers differently raises it, and its first start
// forgets every result reached the old way (freshStart).
//
// 2: servers must carry a real download, not only answer a request.
const serverGen = 2

// freshStart forgets, once per serverGen, the server results kept from
// before: the servers that worked (servers.json) and every test result
// (states.bin), so each server is tested again the new way and none that
// passed on its ping alone is connected to on the strength of it. Every
// list is asked for again at once (their meta files go); the copies of the
// lists stay, for a network where the download fails.
func freshStart(dir string) {
	path := filepath.Join(dir, "server-gen")
	if b, err := os.ReadFile(path); err == nil {
		if g, _ := strconv.Atoi(strings.TrimSpace(string(b))); g >= serverGen {
			return
		}
	}
	for _, f := range []string{"servers.json", "states.bin"} {
		os.Remove(filepath.Join(dir, f))
	}
	metas, _ := filepath.Glob(filepath.Join(dir, "cache", "*.json"))
	for _, m := range metas {
		os.Remove(m)
	}
	os.WriteFile(path, []byte(strconv.Itoa(serverGen)), 0o644)
}

// Start begins scanning (and connects if the user asked for that).
func (e *Engine) Start() {
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.accountChanged()
	go e.refreshReach(e.ctx)
	// A crash left the system pointing at our dead proxy: at today's port,
	// or at the one in use then, which an automatic port change (another
	// program took the port) may have made different.
	for _, addr := range []string{e.proxyAddr(), e.lastProxySet()} {
		if addr != "" && e.opt.SysProxy.Owned(addr) {
			e.opt.SysProxy.Clear()
			break
		}
	}
	e.rememberProxySet("")
	if e.bin.SingBox == "" {
		e.logf("error", "Connection core not found; please reinstall the app.")
	}
	if e.bin.Xray == "" {
		e.logf("warn", "Secondary core not found; some connection types cannot be used.")
	}
	e.logf("info", "%d servers loaded from the last session.", len(e.good))
	e.loadUpdate()
	e.wg.Add(3)
	go e.refreshLoop()
	go e.scanLoop()
	go e.statsLoop()
	e.mu.Lock()
	auto := e.settings.AutoConnect
	e.mu.Unlock()
	if auto {
		go e.Connect(e.target)
	}
}

// Close disconnects, stops all work and saves state.
func (e *Engine) Close() {
	e.disconnect(false)
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
	e.rt.StopTor()
	e.stopHelp()
	e.saveServers()
	e.saveSourceStats()
	e.saveStates()
	e.pool.Close()
}

func (e *Engine) logf(level, format string, args ...any) {
	line := LogLine{Time: time.Now(), Level: level, Text: Redact(fmt.Sprintf(format, args...))}
	e.mu.Lock()
	e.logs = append(e.logs, line)
	if len(e.logs) > 400 {
		e.logs = e.logs[len(e.logs)-400:]
	}
	e.mu.Unlock()
}

// Logs returns recent activity.
func (e *Engine) Logs() []LogLine {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]LogLine(nil), e.logs...)
}

func (e *Engine) setStatus(status, message string) {
	e.mu.Lock()
	e.status, e.message = status, message
	e.mu.Unlock()
}

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// SetLocalDNS changes the resolver cores started from now on use for
// direct traffic (see Options.LocalDNS).
func (e *Engine) SetLocalDNS(addr string) {
	e.mu.Lock()
	moved := e.localDNS != addr
	e.localDNS = addr
	c := e.conn
	e.mu.Unlock()
	// Another network filters differently.
	e.net.use(addr)
	if moved {
		// Probe the new network's regime now.
		e.reprobeRegime("the network changed")
		e.triggerRefill()
	}
	if moved && c != nil {
		go e.resolverMoved(c)
	}
}

// resolverMoved rebuilds the connection with the network's new resolver,
// unless the connection still carries traffic. The cores carry the old
// resolver in their configs: on a phone that moved from Wi-Fi to mobile
// data it is unreachable and every server named by a host fails to
// resolve, so the connection is rebuilt. But the resolver also changes
// while the network stays (a phone with two SIMs reorders its list, or the
// network is read again as the VPN comes up), and rebuilding then only
// dropped a working connection, often a second after it was made. A
// connection that still works stays; the next one uses the new resolver.
func (e *Engine) resolverMoved(c *connection) {
	parent := e.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, resolverCheck)
	r := tester.Probe(ctx, c.proxyURL(), resolverCheck)
	cancel()
	e.mu.Lock()
	current := e.conn == c
	e.mu.Unlock()
	if !current || parent.Err() != nil {
		return
	}
	if r.OK {
		e.logf("info", "The network's resolver changed; the connection still works, so it stays.")
		return
	}
	e.logf("info", "The network changed; reconnecting with its resolver.")
	e.rebuild(c, nil)
}

// resolverCheck is how long a connection has to show it still works after
// the network's resolver changed.
var resolverCheck = 8 * time.Second

func (e *Engine) dns() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.localDNS
}

// tunnelDNS is the resolver DNS tunnels send their queries through (see
// Options.NetworkDNS).
func (e *Engine) tunnelDNS() string {
	if d := e.dns(); d != "" {
		return d
	}
	if e.opt.NetworkDNS != nil {
		return e.opt.NetworkDNS()
	}
	return ""
}

func (e *Engine) proxyAddr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return "127.0.0.1:" + strconv.Itoa(e.settings.ProxyPort)
}

// proxyClient returns an HTTP client through our own proxy while
// connected, so blocked sources (Telegram, GitHub) are fetched through it.
func (e *Engine) proxyClient() *http.Client {
	e.mu.Lock()
	c := e.conn
	e.mu.Unlock()
	if c == nil || !c.ready() {
		return nil
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyURL(c.proxyURL()),
		TLSHandshakeTimeout: 20 * time.Second,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}}
}

// accountHTTP is the account service's client: through the VPN while it
// is up, otherwise direct without showing the service's name where possible.
func (e *Engine) accountHTTP() *http.Client {
	if c := e.proxyClient(); c != nil {
		return c
	}
	return e.privateDirect
}

func (e *Engine) anyClient() *http.Client {
	if c := e.proxyClient(); c != nil {
		return c
	}
	return e.fetcher.Direct
}

func (e *Engine) applyIntensity() {
	switch e.settings.Intensity {
	case "low":
		e.tester.Concurrency, e.tester.TCPConcurrency = 12, 48
	case "max":
		e.tester.Concurrency, e.tester.TCPConcurrency = 64, 256
	default:
		e.tester.Concurrency, e.tester.TCPConcurrency = 32, 128
	}
	e.tester.Timeout = 10 * time.Second
	e.tester.TCPTimeout = 3 * time.Second
}

// newTester returns a copy of the tester for one run, with the current
// intensity and resolver.
func (e *Engine) newTester() tester.Tester {
	tunnel := e.tunnelDNS()
	e.mu.Lock()
	defer e.mu.Unlock()
	t := *e.tester
	if e.wantConnect && e.conn == nil && e.settings.Intensity == "low" {
		// Finding a server is what the user is waiting for; a phone
		// saves its battery once connected, not before.
		t.Concurrency, t.TCPConcurrency = 32, 128
	}
	t.LocalDNS, t.TunnelDNS = e.localDNS, tunnel
	t.Mobile = e.opt.Mobile
	t.LineMbps = e.net.lineMbps()
	// Every server that answers must also carry a real download (scanSpeed).
	t.SpeedBytes, t.SpeedTimeout, t.SpeedNeed, t.SpeedConcurrency = 512<<10, 8*time.Second, unusable, 2
	if e.opt.Mobile {
		// A phone's data is paid for; a quarter of a megabyte still gets
		// past the first kilobytes filtering lets through.
		t.SpeedBytes, t.SpeedConcurrency = 256<<10, 1
	}
	return t
}

// Settings returns the current settings.
func (e *Engine) Settings() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settings
}

// UpdateSettings applies new settings and reconnects when routing changed.
// Turning on what the plan does not have is refused (PlanError).
func (e *Engine) UpdateSettings(s Settings) error {
	if f := e.planLocked(e.Settings(), s); f != "" {
		return e.planError(f)
	}
	e.mu.Lock()
	old := e.settings
	if s.ProxyPort <= 0 || s.ProxyPort > 65535 {
		s.ProxyPort = old.ProxyPort
	}
	s.Country = e.target
	s.Connected = old.Connected
	s.normalize()
	if e.opt.Mobile {
		s.Mode = "proxy"
	}
	e.settings = s
	e.countriesAt = time.Time{} // the list depends on the protocol
	e.applyIntensity()
	connected := e.conn != nil
	e.mu.Unlock()
	if s.Mode == "tun" && old.Mode != "tun" && !e.opt.IsAdmin() {
		e.mu.Lock()
		e.settings.Mode = old.Mode
		e.mu.Unlock()
		return ErrNeedAdmin
	}
	saveJSON(filepath.Join(e.dir, "settings.json"), e.Settings())
	if s.ShareStats && !old.ShareStats {
		go e.crowdLookup(e.ctx)
	}
	if old.HelpOthers != s.HelpOthers {
		go e.updateHelp()
	}
	if s.BlockAds && !old.BlockAds {
		// The ad list is only kept fresh while it is used.
		e.refreshRules()
	}
	if old.DeepSources != s.DeepSources || len(old.UserSources) != len(s.UserSources) || len(old.DisabledSources) != len(s.DisabledSources) {
		e.buildSources()
		e.triggerRefill()
	}
	routing := old.Mode != s.Mode || old.Bypass != s.Bypass || old.Protocol != s.Protocol || old.BlockAds != s.BlockAds || old.SecureOnly != s.SecureOnly ||
		old.ProxyPort != s.ProxyPort || old.AllowLAN != s.AllowLAN || old.Fragment != s.Fragment || old.AllowPlain != s.AllowPlain
	if connected && routing {
		go e.Connect(e.target)
	}
	if e.opt.Autostart != nil && old.StartWithSystem != s.StartWithSystem {
		if err := e.opt.Autostart(s.StartWithSystem); err != nil {
			e.logf("warn", "Could not change the start-with-system setting: %v", err)
		}
	}
	return nil
}

// ErrNeedAdmin means TUN mode needs the app to run as administrator.
var ErrNeedAdmin = fmt.Errorf("need administrator")

func (e *Engine) triggerRefill() {
	select {
	case e.refill <- struct{}{}:
	default:
	}
}

// deviceName is the word for this kind of device that the account service
// shows on the account page's device list.
func deviceName(opt Options) string {
	if opt.Mobile {
		return "Android"
	}
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "Mac"
	case "linux":
		return "Linux"
	}
	return runtime.GOOS
}

// installNumber returns this installation's random number, made once and
// kept in the data folder. It never leaves the device.
func installNumber(dir string) uint32 {
	path := filepath.Join(dir, "install.id")
	if b, err := os.ReadFile(path); err == nil && len(b) == 8 {
		if v, err := strconv.ParseUint(string(b), 16, 32); err == nil {
			return uint32(v)
		}
	}
	var b [4]byte
	rand.Read(b[:])
	v := binary.BigEndian.Uint32(b[:])
	os.WriteFile(path, []byte(fmt.Sprintf("%08x", v)), 0o644)
	return v
}

// buildSources rebuilds the source list from built-ins and settings.
func (e *Engine) buildSources() {
	e.mu.Lock()
	defer e.mu.Unlock()
	disabled := map[string]bool{}
	for _, id := range e.settings.DisabledSources {
		disabled[id] = true
	}
	var list []sources.Source
	builtin := e.opt.Sources
	if builtin == nil {
		builtin = append(append(append(sources.Central(e.install), sources.Account()), sources.Builtin()...), sources.Warp())
	}
	for _, s := range builtin {
		if s.Tier == sources.TierDeep && !s.Central && !e.settings.DeepSources {
			s.Enabled = false
		}
		if ownSource(s) && !e.Can(FeatureOwn) {
			// No key would open them (the service gives it with the plan).
			s.Enabled = false
		}
		if disabled[s.ID] {
			s.Enabled = false
		}
		list = append(list, s)
	}
	list = append(list, e.settings.UserSources...)
	// Keep indexes stable for sources that already exist: the pool stores
	// source indexes.
	if e.srcIndex == nil {
		e.srcIndex = map[string]int{}
	}
	byID := map[string]sources.Source{}
	for _, s := range list {
		byID[s.ID] = s
	}
	for i := range e.srcs {
		if s, ok := byID[e.srcs[i].ID]; ok {
			e.srcs[i] = s
			delete(byID, s.ID)
		} else {
			e.srcs[i].Enabled = false
		}
	}
	for _, s := range list {
		if _, isNew := byID[s.ID]; !isNew {
			continue
		}
		e.srcIndex[s.ID] = len(e.srcs)
		e.srcs = append(e.srcs, s)
		e.srcStats = append(e.srcStats, e.srcStatsByID[s.ID])
	}
}

// CountryInfo summarizes servers in one country.
type CountryInfo struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Flag      string `json:"flag"`
	Servers   int    `json:"servers"`
	BestDelay int    `json:"best_delay"`
}

// Countries lists the countries that have usable servers for the chosen
// protocol, the best supplied first.
func (e *Engine) Countries() []CountryInfo {
	now := time.Now()
	e.mu.Lock()
	if now.Sub(e.countriesAt) < countryCache {
		out := e.countries
		e.mu.Unlock()
		return out
	}
	m := map[string]*CountryInfo{}
	for _, s := range e.good {
		if s.Country == "" || s.Country == "IR" || s.Proto == "direct" || !s.matches(e.settings.Protocol) || !s.usable(now) {
			continue
		}
		ci, ok := m[s.Country]
		if !ok {
			c, _ := geo.Lookup(s.Country)
			ci = &CountryInfo{Code: s.Country, Name: c.EN, Flag: geo.Flag(s.Country)}
			m[s.Country] = ci
		}
		ci.Servers++
		if ci.BestDelay == 0 || s.Delay < ci.BestDelay {
			ci.BestDelay = s.Delay
		}
	}
	e.mu.Unlock()
	out := make([]CountryInfo, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Servers != out[j].Servers {
			return out[i].Servers > out[j].Servers
		}
		return out[i].Code < out[j].Code
	})
	e.mu.Lock()
	e.countries, e.countriesAt = out, now
	e.mu.Unlock()
	return out
}

// countryCache is how long the country list is reused. The UI asks every
// second and the list changes slowly.
const countryCache = 3 * time.Second

// ServerInfo is a working server as shown in the UI.
type ServerInfo struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Proto     string `json:"proto"`
	Transport string `json:"transport"`
	Security  string `json:"security"`
	Country   string `json:"country"`
	Delay     int    `json:"delay"`
	AgeSec    int    `json:"age_sec"`
	Fragment  bool   `json:"fragment"`
	Plain     bool   `json:"plain"`
	Active    bool   `json:"active"`
	Link      string `json:"link"`
}

// Servers lists usable servers in a country ("" = all), best first.
func (e *Engine) Servers(country string, limit int) []ServerInfo {
	now := time.Now()
	e.mu.Lock()
	active := map[string]bool{}
	if e.conn != nil {
		active[e.conn.activeKey()] = true
	}
	var list []*Server
	for _, s := range e.good {
		if !s.usable(now) || (country != "" && s.Country != country) {
			continue
		}
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].rank(now) < list[j].rank(now) })
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	out := make([]ServerInfo, 0, len(list))
	for _, s := range list {
		out = append(out, ServerInfo{
			Key: s.Key, Name: s.Name, Proto: s.Proto, Transport: s.Transport, Security: s.Security,
			Country: s.Country, Delay: s.Delay, AgeSec: int(now.Sub(s.LastOK).Seconds()),
			Fragment: s.Fragment, Plain: s.Plain, Active: active[s.Key], Link: s.Link,
		})
	}
	e.mu.Unlock()
	return out
}

// domesticEndpoint reports whether a server's address is inside the
// country: in the country's address ranges or, in the project's own sealed
// lists (own says the server came from one; the relays are there), named
// as a relay. A name in a public list says nothing: anyone may call a
// server "x-relay", and a relay jumps the queue when foreign addresses are
// cut.
func (e *Engine) domesticEndpoint(n *node.Node, own bool) bool {
	if n == nil {
		return false
	}
	if a, err := netip.ParseAddr(n.Server); err == nil && ipIn(e.irRanges, a) {
		return true
	}
	return own && (strings.HasSuffix(n.Name, "-relay") || n.Name == "relay")
}

// fetchMode is how sources are reached now: as a network where only
// domestic addresses answer allows, when that is what the regime found.
func (e *Engine) fetchMode() sources.Mode {
	e.mu.Lock()
	r := e.reg.Regime
	e.mu.Unlock()
	if r.severity() >= regimeAllowlist.severity() {
		return sources.ModeDomestic
	}
	return sources.ModeNormal
}

// domesticMirrors returns the mirror bases learned from share codes.
func (e *Engine) domesticMirrors() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.settings.DomesticMirrors...)
}
