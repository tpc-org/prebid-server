package activitylog

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/modules/moduledeps"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Documentation ranges stand in for real addresses.
const (
	dcUser    = "198.51.100.7"  // in the dc list
	relayUser = "203.0.113.9"   // Apple Private Relay (also inside a dc range)
	homeUser  = "192.0.2.44"    // clean
	forwarder = "198.51.100.99" // a server-side publisher / dsp-connector host
	chromeUA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36"
)

func writeIVTDir(t *testing.T, withKey bool) string {
	t.Helper()
	d := t.TempDir()
	files := map[string]string{
		"dc-v4.txt":        "198.51.100.0/24\n203.0.113.0/24\n0.0.0.0/0\n",
		"dc-v6.txt":        "2001:db8:dc::/48\n",
		"crawler.txt":      "192.0.2.200/32\n",
		"relay.txt":        "203.0.113.0/28\n",
		"ivt-allow.manual": "198.51.100.99   # test forwarder\n",
		"ivt-block.manual": "192.0.2.250   # test incident\n",
		"badua.txt":        "proximic\nAhrefsBot\nNinja\n",
	}
	if withKey {
		files["hashkey"] = "2026w46 00112233445566778899aabbccddeeff\n"
	}
	for n, c := range files {
		require.NoError(t, os.WriteFile(filepath.Join(d, n), []byte(c), 0644))
	}
	return d
}

func buildIVTModule(t *testing.T, logDir, ivtDir string) Module {
	t.Helper()
	cfg, err := json.Marshal(map[string]interface{}{"enabled": true, "log_dir": logDir, "ivt_dir": ivtDir})
	require.NoError(t, err)
	built, err := Builder(cfg, moduledeps.ModuleDeps{})
	require.NoError(t, err)
	return built.(Module)
}

// runAuction drives entrypoint -> processed -> response the way the hook
// executor does, merging each stage's ModuleContext into the module's one.
func runAuction(t *testing.T, m Module, body string, xff string, req *openrtb2.BidRequest) map[string]interface{} {
	t.Helper()
	httpReq := httptest.NewRequest("POST", "/openrtb2/auction", strings.NewReader(body))
	if xff != "" {
		httpReq.Header.Set("X-Forwarded-For", xff)
	}
	entry, err := m.HandleEntrypointHook(context.Background(), hookstage.ModuleInvocationContext{},
		hookstage.EntrypointPayload{Request: httpReq, Body: []byte(body)})
	require.NoError(t, err)
	mc := entry.ModuleContext
	if mc == nil {
		mc = hookstage.NewModuleContext()
	}
	req.ID = "a1"
	req.Imp = []openrtb2.Imp{impWithBidders("imp1", "pub-1234abcd", "thrad")}
	proc, err := m.HandleProcessedAuctionHook(context.Background(), hookstage.ModuleInvocationContext{ModuleContext: mc},
		hookstage.ProcessedAuctionRequestPayload{Request: &openrtb_ext.RequestWrapper{BidRequest: req}})
	require.NoError(t, err)
	mc.SetAll(proc.ModuleContext.GetAll())
	_, err = m.HandleAuctionResponseHook(context.Background(), hookstage.ModuleInvocationContext{ModuleContext: mc},
		hookstage.AuctionResponsePayload{BidResponse: &openrtb2.BidResponse{}})
	require.NoError(t, err)
	lines := readLines(t, m.cfg.LogDir, "activity")
	require.NotEmpty(t, lines)
	return lines[len(lines)-1]
}

func TestIPRangesLookupAndSanityLimit(t *testing.T) {
	l := loadIVTLists(writeIVTDir(t, true))
	assert.Equal(t, "dc", l.ipClass(netip.MustParseAddr(dcUser)))
	assert.Equal(t, "relay", l.ipClass(netip.MustParseAddr(relayUser)), "exemption wins over dc")
	assert.Equal(t, "forwarder", l.ipClass(netip.MustParseAddr(forwarder)))
	assert.Equal(t, "manual", l.ipClass(netip.MustParseAddr("192.0.2.250")))
	assert.Equal(t, "crawler", l.ipClass(netip.MustParseAddr("192.0.2.200")))
	assert.Equal(t, "dc", l.ipClass(netip.MustParseAddr("2001:db8:dc::5")))
	assert.Equal(t, "dc", l.ipClass(netip.MustParseAddr("::ffff:198.51.100.7")), "IPv4-mapped is unmapped")
	// 0.0.0.0/0 in the file is wider than /8 and must be ignored
	assert.Equal(t, "clean", l.ipClass(netip.MustParseAddr(homeUser)))
	assert.Equal(t, "clean", l.ipClass(netip.MustParseAddr("2001:db8:1::1")))
}

func TestClassifyUA(t *testing.T) {
	bad := compileBadUA(writeIVTDir(t, false))
	cases := []struct{ ua, kind, family string }{
		{chromeUA, "browser", "chrome"},
		{"Mozilla/5.0 (Linux; Android 14; K; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/157.0 Mobile Safari/537.36", "browser", "chrome-webview"},
		{"undici", "http-client", ""},
		{"Go-http-client/2.0", "http-client", ""},
		{"Dalvik/2.1.0 (Linux; U; Android 14)", "http-client", ""},
		{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/156.0.0.0 Safari/537.36", "automation", "chrome"},
		{"Mozilla/5.0 (compatible; proximic; +https://www.comscore.com/Web-Crawler)", "automation", ""},
		{"Mozilla/5.0 (compatible; AhrefsBot/7.0)", "automation", ""},
		{"NinjaFruitGame/3.2 (iPhone)", "other", ""},
		{"Ninja/1.0 (scraper)", "bad-ua", ""},
		{"", "empty", ""},
	}
	for _, c := range cases {
		kind, family, _ := classifyUA(c.ua, bad)
		assert.Equal(t, c.kind, kind, c.ua)
		assert.Equal(t, c.family, family, c.ua)
	}
	_, _, major := classifyUA(chromeUA, bad)
	assert.Equal(t, 157, major)
}

func TestBrowserAuctionGetsEndUserFields(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), writeIVTDir(t, true))
	line := runAuction(t, m, `{"site":{"page":"https://www.pub.example/x"}}`, dcUser, &openrtb2.BidRequest{
		Site:   &openrtb2.Site{Page: "https://www.pub.example/x"},
		Device: &openrtb2.Device{IP: dcUser, UA: chromeUA}, // PBS filled these from headers
	})
	u := line["u"].(map[string]interface{})
	assert.Equal(t, "site", u["src"])
	assert.Equal(t, "pub.example", u["dom"])
	assert.Equal(t, "dc", u["ipc"])
	assert.Equal(t, "198.51.100.0/24", u["p24"])
	assert.Len(t, u["iph"], 16)
	assert.Equal(t, "2026w46", u["hk"])
	assert.Equal(t, "browser", u["uak"])
	assert.Equal(t, "header", u["ipsrc"], "a browser's IP always comes from headers")
	assert.Nil(t, u["fwd"])
	raw, _ := json.Marshal(line)
	assert.NotContains(t, string(raw), dcUser, "the IP itself is never written")
	assert.NotContains(t, string(raw), "Chrome/157", "the raw UA is never written")
}

func TestServerSideJudgesEndUserNotForwarder(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), writeIVTDir(t, true))
	body := `{"device":{"ip":"192.0.2.44","ua":"` + chromeUA + `"},"site":{"domain":"pub.example"}}`
	line := runAuction(t, m, body, forwarder, &openrtb2.BidRequest{
		Site:   &openrtb2.Site{Domain: "pub.example"},
		Device: &openrtb2.Device{IP: homeUser, UA: chromeUA},
	})
	u := line["u"].(map[string]interface{})
	assert.Equal(t, "clean", u["ipc"], "classified on the end user's IP")
	assert.Equal(t, "body", u["ipsrc"])
	assert.Equal(t, "body", u["uasrc"])
	assert.Equal(t, true, u["fwd"], "connection came from an allowlisted forwarder")
}

func TestServerSideWithoutDeviceIsFlaggedAsHeaderFallback(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), writeIVTDir(t, true))
	// The caller sent no device object: PBS fills device.ip/ua from the
	// forwarder's own connection, which is what partners would then see.
	line := runAuction(t, m, `{"site":{"domain":"pub.example"}}`, forwarder, &openrtb2.BidRequest{
		Site:   &openrtb2.Site{Domain: "pub.example"},
		Device: &openrtb2.Device{IP: forwarder, UA: "undici"},
	})
	u := line["u"].(map[string]interface{})
	assert.Equal(t, "header", u["ipsrc"])
	assert.Equal(t, "header", u["uasrc"])
	assert.Equal(t, "forwarder", u["ipc"])
	assert.Equal(t, "http-client", u["uak"])
	assert.Equal(t, true, u["fwd"])
}

func TestAppTrafficBundleAndIFA(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), writeIVTDir(t, true))
	body := `{"app":{"bundle":"com.example.app"},"device":{"ip":"192.0.2.44","ifa":"AAAA-1111"}}`
	line := runAuction(t, m, body, forwarder, &openrtb2.BidRequest{
		App:    &openrtb2.App{Bundle: "com.example.app"},
		Device: &openrtb2.Device{IP: homeUser, IFA: "AAAA-1111", UA: "Dalvik/2.1.0 (Linux; U; Android 14)"},
	})
	u := line["u"].(map[string]interface{})
	assert.Equal(t, "app", u["src"])
	assert.Equal(t, "com.example.app", u["bundle"])
	assert.Len(t, u["ifh"], 16)
	assert.NotContains(t, mustJSON(line), "AAAA-1111")

	line = runAuction(t, m, body, forwarder, &openrtb2.BidRequest{
		App:    &openrtb2.App{Bundle: "com.example.app"},
		Device: &openrtb2.Device{IP: homeUser, IFA: "00000000-0000-0000-0000-000000000000"},
	})
	u = line["u"].(map[string]interface{})
	assert.Equal(t, true, u["ifz"])
	assert.Nil(t, u["ifh"])
}

func TestNoKeyMeansNoHashes(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), writeIVTDir(t, false))
	line := runAuction(t, m, `{}`, dcUser, &openrtb2.BidRequest{Device: &openrtb2.Device{IP: dcUser}})
	u := line["u"].(map[string]interface{})
	assert.Nil(t, u["iph"])
	assert.Nil(t, u["hk"])
	assert.Equal(t, "dc", u["ipc"])
}

func TestWithoutIVTDirTheLineIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	m := buildModule(t, dir, "")
	_, err := m.HandleEntrypointHook(context.Background(), hookstage.ModuleInvocationContext{},
		hookstage.EntrypointPayload{Body: []byte(`{}`)})
	require.NoError(t, err)
	result := processRequest(t, m, []openrtb2.Imp{impWithBidders("imp1", "pub-1234abcd", "thrad")})
	processResponse(t, m, result.ModuleContext, &openrtb2.BidResponse{})
	lines := readLines(t, dir, "activity")
	require.Len(t, lines, 1)
	_, has := lines[0]["u"]
	assert.False(t, has)
}

func TestMissingIVTDirIsNotFatal(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), filepath.Join(t.TempDir(), "does-not-exist"))
	line := runAuction(t, m, `{}`, dcUser, &openrtb2.BidRequest{Device: &openrtb2.Device{IP: dcUser, UA: chromeUA}})
	u := line["u"].(map[string]interface{})
	assert.Nil(t, u["ipc"], "no lists loaded: no class, rather than a misleading 'clean'")
	assert.Equal(t, "browser", u["uak"])
}

func TestUAHashDistinguishesDevicesButHidesTheUA(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), writeIVTDir(t, true))
	a := runAuction(t, m, `{}`, homeUser, &openrtb2.BidRequest{Device: &openrtb2.Device{IP: homeUser, UA: chromeUA}})
	b := runAuction(t, m, `{}`, homeUser, &openrtb2.BidRequest{Device: &openrtb2.Device{IP: homeUser,
		UA: strings.Replace(chromeUA, "Windows NT 10.0; Win64; x64", "Macintosh; Intel Mac OS X 10_15_7", 1)}})
	ua, ub := a["u"].(map[string]interface{}), b["u"].(map[string]interface{})
	assert.Len(t, ua["uah"], 8)
	assert.NotEqual(t, ua["uah"], ub["uah"], "same family+major, different device strings")
	assert.Equal(t, ua["uaf"], ub["uaf"])
}

func TestListsReloadInBackgroundWhenFilesChange(t *testing.T) {
	d := writeIVTDir(t, true)
	s := newIVTStore(d)
	require.Equal(t, "clean", s.get(time.Now()).ipClass(netip.MustParseAddr(homeUser)))
	require.NoError(t, os.WriteFile(filepath.Join(d, "ivt-block.manual"), []byte("192.0.2.0/24 # new incident\n"), 0644))
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(d, "ivt-block.manual"), future, future))
	s.get(time.Now().Add(ivtReloadInterval + time.Minute)) // triggers a background reload
	assert.Eventually(t, func() bool {
		return s.get(time.Now()).ipClass(netip.MustParseAddr(homeUser)) == "manual"
	}, 2*time.Second, 20*time.Millisecond)
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
