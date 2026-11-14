package activitylog

// ── End-user IVT signals (HOL-141 / HOL-143), monitor only ─────────────────
//
// IVT is judged on the END USER described in the bid request, for every
// source: browser, server-side publishers, dsp-connector (DSP traffic) and
// in-app. The forwarding server's own connection IP (a cloud/data-centre
// address) is never the thing being judged. nginx only sees that
// connection, so the end-user view has to come from here, where the parsed
// request (device.ip/ua/ifa, site/app) is available before any bidder runs.
//
// When cfg.IVTDir is set, each activity.log line gains a `u` object:
//
//	src     "site" | "app"
//	dom     site.domain (or site.page host), www. stripped      — publisher-supplied, not personal
//	bundle  app.bundle                                          — publisher-supplied, not personal
//	ipc     list class of device.ip, decided IN-PROCESS: relay | forwarder | manual | crawler | dc | clean
//	iph     keyed hash of device.ip (HMAC-SHA256, 16 hex). The key rotates
//	        (pbs-settings nginx/ivt publishes it) and is then discarded, so a
//	        hash can't be reversed or linked across key periods.
//	hk      id of the key period that produced iph/ifh
//	p24     device.ip /24 (IPv4) or /48 (IPv6) prefix (decided 2026-11-13)
//	uak     device.ua kind: browser | http-client | automation | bad-ua | empty | other
//	uaf/uav device.ua family and major version
//	uah     short keyed hash of the exact device.ua (one-device dominance)
//	ifh/ifz keyed hash of device.ifa / ifa is all zeros
//	ipsrc   where device.ip came from: body | header (PBS fell back to the
//	        request headers, i.e. the caller didn't send it) | none
//	uasrc   same for device.ua
//	fwd     the connection came from a known forwarder (ivt-allow.manual)
//	blk     the reason this auction WOULD be blocked ("" = not), always set
//	enf     the auction WAS rejected (enforcement on for its placement)
//
// No full IP and no raw UA is ever written. Nothing here blocks: verdicts
// are computed from these fields by pbs-settings' nginx/ivt/ivt_report.py.
// A missing or unreadable IVT directory is never fatal: the line just has
// no `u` (or a partial one).

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buger/jsonparser"
	"github.com/golang/glog"
	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
)

const (
	entryContextKey   = "activitylog-entry"
	ivtReloadInterval = 10 * time.Minute
	maxFieldLen       = 200
)

// File names inside cfg.IVTDir, as published by pbs-settings nginx/ivt.
var ivtFiles = struct {
	relay, allow, manual, crawler, dc, badUA []string
	key                                      string
}{
	relay:   []string{"relay.txt"},
	allow:   []string{"ivt-allow.manual"},
	manual:  []string{"ivt-block.manual"},
	crawler: []string{"crawler.txt", "monitoring.txt"},
	dc:      []string{"dc-v4.txt", "dc-v6.txt"},
	badUA:   []string{"badua.txt"},
	key:     "hashkey",
}

// ── IP ranges ──────────────────────────────────────────────────────────────

type ipRange struct{ start, end netip.Addr }

// ipRanges is a sorted, merged list of address ranges with O(log n) lookup.
type ipRanges []ipRange

func (r ipRanges) contains(a netip.Addr) bool {
	if !a.IsValid() || len(r) == 0 {
		return false
	}
	a = a.Unmap()
	i := sort.Search(len(r), func(i int) bool { return r[i].start.Compare(a) > 0 }) - 1
	return i >= 0 && r[i].end.Compare(a) >= 0
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	bits := p.Bits()
	for i := range b {
		for j := 0; j < 8; j++ {
			if i*8+j >= bits {
				b[i] |= 0x80 >> j
			}
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// minPrefix is the sanity limit pbs-settings' ivt_lists.py also enforces: an
// entry wider than this is ignored, so a bad file can't classify everything.
func minPrefix(a netip.Addr) int {
	if a.Is4() {
		return 8
	}
	return 19
}

func parsePrefixLine(line string) (netip.Prefix, bool) {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	f := strings.Fields(strings.Split(line, ",")[0])
	if len(f) == 0 {
		return netip.Prefix{}, false
	}
	tok := f[0]
	if p, err := netip.ParsePrefix(tok); err == nil {
		if p.Addr().Is4In6() {
			return netip.Prefix{}, false // lists use plain IPv4; a mapped prefix is a malformed entry
		}
		return p, p.Bits() >= minPrefix(p.Addr())
	}
	if a, err := netip.ParseAddr(tok); err == nil {
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

func loadRanges(dir string, names []string) ipRanges {
	var r ipRanges
	for _, n := range names {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if p, ok := parsePrefixLine(sc.Text()); ok {
				r = append(r, ipRange{p.Masked().Addr(), lastAddr(p)})
			}
		}
		f.Close()
	}
	sort.Slice(r, func(i, j int) bool { return r[i].start.Compare(r[j].start) < 0 })
	merged := r[:0]
	for _, x := range r {
		if n := len(merged); n > 0 && merged[n-1].start.BitLen() == x.start.BitLen() &&
			(x.start.Compare(merged[n-1].end) <= 0 || x.start == merged[n-1].end.Next()) {
			if x.end.Compare(merged[n-1].end) > 0 {
				merged[n-1].end = x.end
			}
			continue
		}
		merged = append(merged, x)
	}
	return merged
}

// ── User agents (mirrors pbs-settings nginx/ivt/ivtlib.py) ─────────────────

var (
	serverUA = regexp.MustCompile(`(?i)^(undici|node-fetch|node|axios/|got |go-http-client|okhttp|dalvik/|cfnetwork|` +
		`java/|apache-httpclient|python-requests|python-urllib|aiohttp|httpx|curl/|wget/|postmanruntime|insomnia|` +
		`libwww-perl|guzzle|php|ruby|restsharp|\.net|deno|bun/)`)
	automationUA = regexp.MustCompile(`(?i)headlesschrome|phantomjs|lighthouse|puppeteer|playwright|selenium|scrapy|bot\b|spider|crawler`)
	uaFamilies   = []struct {
		name string
		re   *regexp.Regexp
	}{
		{"edge", regexp.MustCompile(`Edg[AeiOS]*/(\d+)`)},
		{"samsung", regexp.MustCompile(`SamsungBrowser/(\d+)`)},
		{"firefox", regexp.MustCompile(`(?:Firefox|FxiOS)/(\d+)`)},
		{"chrome-ios", regexp.MustCompile(`CriOS/(\d+)`)},
		{"chrome", regexp.MustCompile(`Chrome/(\d+)`)},
		{"safari", regexp.MustCompile(`Version/(\d+)[.\d]* (?:Mobile/\S+ )?Safari/`)},
	}
)

// compileBadUA builds a matcher from bad-bot UA tokens. Long tokens (>= 6
// chars) match as whole words; short ones only as a product token (start
// of the UA, or immediately before '/'), because short words like "Ninja"
// can be real app names inside in-app UAs. Go's RE2 has no lookbehind, so
// word boundaries are spelled out.
func compileBadUA(dir string) *regexp.Regexp {
	var long, short []string
	for _, n := range ivtFiles.badUA {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			if len(t) < 3 {
				continue
			}
			if len(t) >= 6 {
				long = append(long, regexp.QuoteMeta(t))
			} else {
				short = append(short, regexp.QuoteMeta(t))
			}
		}
	}
	var parts []string
	if len(long) > 0 {
		parts = append(parts, `(?:^|[^a-z0-9])(?:`+strings.Join(long, "|")+`)(?:$|[^a-z0-9])`)
	}
	if len(short) > 0 {
		alt := strings.Join(short, "|")
		parts = append(parts, `(?:^|[\s(;])(?:`+alt+`)/`, `^(?:`+alt+`)(?:$|[^a-z0-9])`)
	}
	if len(parts) == 0 {
		return nil
	}
	re, err := regexp.Compile(`(?i)` + strings.Join(parts, "|"))
	if err != nil {
		glog.Warningf("activitylog: bad-UA list did not compile: %v", err)
		return nil
	}
	return re
}

func classifyUA(ua string, badUA *regexp.Regexp) (kind, family string, major int) {
	ua = strings.TrimSpace(ua)
	switch {
	case ua == "" || ua == "-":
		return "empty", "", 0
	case serverUA.MatchString(ua):
		kind = "http-client"
	case automationUA.MatchString(ua):
		kind = "automation"
	case badUA != nil && badUA.MatchString(ua):
		kind = "bad-ua"
	case strings.HasPrefix(ua, "Mozilla/"):
		kind = "browser"
	default:
		kind = "other"
	}
	for _, f := range uaFamilies {
		if m := f.re.FindStringSubmatch(ua); m != nil {
			family = f.name
			if f.name == "chrome" && strings.Contains(ua, "; wv)") {
				family = "chrome-webview"
			}
			major, _ = strconv.Atoi(m[1])
			break
		}
	}
	return kind, family, major
}

// ── The loaded state, swapped atomically on reload ─────────────────────────

type ivtLists struct {
	relay, allow, manual, crawler, dc ipRanges
	badUA                             *regexp.Regexp
	keyID                             string
	key                               []byte
	// loaded is false when no IP list could be read (directory missing or
	// not yet published): ipc is then left empty instead of claiming "clean".
	loaded bool
	// Enforcement (enforce.go): off unless ivt-enforce.json says otherwise.
	enforceOn      bool
	enforceLabels  map[string]struct{}
	enforceExclude map[string]struct{}
	breakerLabels  map[string]struct{}
	confirmed      map[string]struct{}
}

func loadIVTLists(dir string) *ivtLists {
	l := &ivtLists{
		relay:   loadRanges(dir, ivtFiles.relay),
		allow:   loadRanges(dir, ivtFiles.allow),
		manual:  loadRanges(dir, ivtFiles.manual),
		crawler: loadRanges(dir, ivtFiles.crawler),
		dc:      loadRanges(dir, ivtFiles.dc),
		badUA:   compileBadUA(dir),
	}
	l.loaded = len(l.relay)+len(l.allow)+len(l.manual)+len(l.crawler)+len(l.dc) > 0
	loadEnforcement(dir, l)
	// hashkey: one line "<key id> <hex key>" (>= 16 bytes). Missing or short
	// key: no iph/ifh is written rather than hashing with a weak key.
	if data, err := os.ReadFile(filepath.Join(dir, ivtFiles.key)); err == nil {
		if f := strings.Fields(string(data)); len(f) == 2 {
			if k, err := hex.DecodeString(f[1]); err == nil && len(k) >= 16 {
				l.keyID, l.key = f[0], k
			}
		}
	}
	return l
}

func (l *ivtLists) ipClass(a netip.Addr) string {
	switch {
	case !a.IsValid() || !l.loaded:
		return ""
	case l.relay.contains(a):
		return "relay"
	case l.allow.contains(a):
		return "forwarder"
	case l.manual.contains(a):
		return "manual"
	case l.crawler.contains(a):
		return "crawler"
	case l.dc.contains(a):
		return "dc"
	}
	return "clean"
}

func (l *ivtLists) hash(v string) string {
	if len(l.key) == 0 || v == "" {
		return ""
	}
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte(v))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// ivtStore reloads the directory in the background when any file in it
// changes, checked at most every ivtReloadInterval. Requests never wait on
// a reload; they use whatever was loaded last.
type ivtStore struct {
	dir       string
	cur       atomic.Pointer[ivtLists]
	mu        sync.Mutex
	lastCheck time.Time
	mtimeSig  string
	loading   atomic.Bool
}

func newIVTStore(dir string) *ivtStore {
	if dir == "" {
		return nil
	}
	s := &ivtStore{dir: dir}
	s.mtimeSig = s.signature()
	s.cur.Store(loadIVTLists(dir))
	s.lastCheck = time.Now()
	return s
}

func (s *ivtStore) signature() string {
	var b strings.Builder
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			b.WriteString(e.Name())
			b.WriteString(info.ModTime().UTC().Format(time.RFC3339Nano))
			b.WriteString(strconv.FormatInt(info.Size(), 10))
		}
	}
	return b.String()
}

func (s *ivtStore) get(now time.Time) *ivtLists {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	stale := now.Sub(s.lastCheck) > ivtReloadInterval
	if stale {
		s.lastCheck = now
	}
	s.mu.Unlock()
	if stale && s.loading.CompareAndSwap(false, true) {
		go func() {
			defer s.loading.Store(false)
			if sig := s.signature(); sig != s.mtimeSig {
				s.cur.Store(loadIVTLists(s.dir))
				s.mtimeSig = sig
			}
		}()
	}
	return s.cur.Load()
}

// ── Entrypoint stage: what only the raw request shows ───────────────────────

type entrySignals struct {
	BodyIP bool
	BodyUA bool
	Peer   netip.Addr
}

// HandleEntrypointHook records whether the caller sent device.ip/ua in the
// body (PBS fills them from headers later if not) and the connecting peer:
// the rightmost X-Forwarded-For entry, which the ALB appends. The peer is
// only ever compared against the forwarder allowlist, never logged.
func (m Module) HandleEntrypointHook(
	_ context.Context,
	_ hookstage.ModuleInvocationContext,
	payload hookstage.EntrypointPayload,
) (hookstage.HookResult[hookstage.EntrypointPayload], error) {
	result := hookstage.HookResult[hookstage.EntrypointPayload]{}
	if !m.cfg.Enabled || m.ivt == nil {
		return result, nil
	}
	sig := entrySignals{
		BodyIP: jsonString(payload.Body, "device", "ip") != "" || jsonString(payload.Body, "device", "ipv6") != "",
		BodyUA: jsonString(payload.Body, "device", "ua") != "",
	}
	if payload.Request != nil {
		sig.Peer = connectingPeer(payload.Request.Header.Get("X-Forwarded-For"), payload.Request.RemoteAddr)
	}
	mc := hookstage.NewModuleContext()
	mc.Set(entryContextKey, sig)
	result.ModuleContext = mc
	return result, nil
}

// connectingPeer is the address that connected to our edge: the RIGHTMOST
// public entry of X-Forwarded-For. In production the chain reaching PBS is
// "<whatever the caller sent>, <peer appended by the ALB>, <ALB's private
// address appended by nginx>", so the last entry is our own load balancer;
// private/loopback hops are ours and are skipped. Falls back to RemoteAddr.
func connectingPeer(xff, remoteAddr string) netip.Addr {
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if a, err := netip.ParseAddr(strings.TrimSpace(parts[i])); err == nil {
			a = a.Unmap()
			if a.IsGlobalUnicast() && !a.IsPrivate() {
				return a
			}
		}
	}
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

func jsonString(body []byte, keys ...string) string {
	v, err := jsonparser.GetString(body, keys...)
	if err != nil {
		return ""
	}
	return v
}

// ── Building the `u` object at ProcessedAuctionRequest ──────────────────────

type endUserLine struct {
	Src    string `json:"src,omitempty"`
	Dom    string `json:"dom,omitempty"`
	Bundle string `json:"bundle,omitempty"`
	IPC    string `json:"ipc,omitempty"`
	IPH    string `json:"iph,omitempty"`
	HK     string `json:"hk,omitempty"`
	P24    string `json:"p24,omitempty"`
	UAK    string `json:"uak,omitempty"`
	UAF    string `json:"uaf,omitempty"`
	UAV    int    `json:"uav,omitempty"`
	UAH    string `json:"uah,omitempty"` // keyed hash of the exact UA: per-device dominance without storing the UA
	IFH    string `json:"ifh,omitempty"`
	IFZ    bool   `json:"ifz,omitempty"`
	IPSrc  string `json:"ipsrc,omitempty"`
	UASrc  string `json:"uasrc,omitempty"`
	Fwd    bool   `json:"fwd,omitempty"`
	Blk    string `json:"blk,omitempty"` // shadow verdict: why it WOULD be blocked (enforce.go)
	Enf    bool   `json:"enf,omitempty"` // true only when this auction was actually rejected
}

func clip(s string) string {
	if len(s) > maxFieldLen {
		return s[:maxFieldLen]
	}
	return s
}

func siteDomain(site *openrtb2.Site) string {
	d := site.Domain
	if d == "" && site.Page != "" {
		if u, err := url.Parse(site.Page); err == nil {
			d = u.Hostname()
		}
	}
	return clip(strings.TrimPrefix(strings.ToLower(d), "www."))
}

func prefixOf(a netip.Addr) string {
	bits := 24
	if a.Is6() {
		bits = 48
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return ""
	}
	return p.String()
}

func source(sig *entrySignals, inBody, present bool) string {
	switch {
	case !present:
		return "none"
	case sig == nil:
		return "" // entrypoint hook not in the execution plan: unknown
	case inBody:
		return "body"
	}
	return "header"
}

func (m Module) endUser(req *openrtb2.BidRequest, sig *entrySignals) *endUserLine {
	l := m.ivt.get(time.Now())
	if l == nil || req == nil {
		return nil
	}
	u := &endUserLine{}
	switch {
	case req.App != nil:
		u.Src, u.Bundle = "app", clip(req.App.Bundle)
	case req.Site != nil:
		u.Src, u.Dom = "site", siteDomain(req.Site)
	}
	var ipStr, ua, ifa string
	if d := req.Device; d != nil {
		ipStr, ua, ifa = d.IP, d.UA, d.IFA
		if ipStr == "" {
			ipStr = d.IPv6
		}
	}
	if a, err := netip.ParseAddr(ipStr); err == nil {
		a = a.Unmap()
		u.IPC = l.ipClass(a)
		u.P24 = prefixOf(a)
		u.IPH = l.hash(a.String())
	}
	u.UAK, u.UAF, u.UAV = classifyUA(ua, l.badUA)
	if h := l.hash("ua:" + ua); ua != "" && h != "" {
		u.UAH = h[:8]
	}
	if ifa != "" {
		if strings.Trim(ifa, "0-") == "" {
			u.IFZ = true
		} else {
			u.IFH = l.hash("ifa:" + strings.ToLower(ifa))
		}
	}
	if u.IPH != "" || u.IFH != "" || u.UAH != "" {
		u.HK = l.keyID
	}
	if sig != nil {
		u.IPSrc = source(sig, sig.BodyIP, ipStr != "")
		u.UASrc = source(sig, sig.BodyUA, ua != "")
		u.Fwd = l.allow.contains(sig.Peer)
	} else {
		u.IPSrc = source(nil, false, ipStr != "")
		u.UASrc = source(nil, false, ua != "")
	}
	return u
}

func entryFrom(mc *hookstage.ModuleContext) *entrySignals {
	if v, ok := mc.Get(entryContextKey); ok {
		if s, ok := v.(entrySignals); ok {
			return &s
		}
	}
	return nil
}
