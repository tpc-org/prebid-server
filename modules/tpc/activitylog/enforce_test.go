package activitylog

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlockReasonMirrorsMonitorPolicy(t *testing.T) {
	l := &ivtLists{confirmed: toSet([]string{"2026w46:bot"})}
	cases := []struct {
		name string
		u    endUserLine
		want string
	}{
		{"unverifiable forwarded is never blocked", endUserLine{Fwd: true, IPSrc: "header", IPC: "manual"}, ""},
		{"unverifiable UA is never blocked", endUserLine{Fwd: true, IPSrc: "body", UASrc: "header", UAK: "automation"}, ""},
		{"private relay exempt", endUserLine{IPC: "relay", UAK: "automation"}, ""},
		{"forwarder address exempt", endUserLine{IPC: "forwarder"}, ""},
		{"incident list", endUserLine{IPC: "manual"}, "manual"},
		{"crawler", endUserLine{IPC: "crawler"}, "crawler"},
		{"automation UA", endUserLine{IPC: "clean", UAK: "automation"}, "ua"},
		{"bad-bot UA", endUserLine{IPC: "", UAK: "bad-ua"}, "ua"},
		{"confirmed dc bot", endUserLine{IPC: "dc", IPH: "bot", HK: "2026w46"}, "behaviour-dc"},
		{"confirmed clean bot", endUserLine{IPC: "clean", IPH: "bot", HK: "2026w46"}, "behaviour-clean"},
		{"other week's hash never matches", endUserLine{IPC: "dc", IPH: "bot", HK: "2026w47"}, ""},
		{"dc on probation", endUserLine{IPC: "dc", IPH: "someone", HK: "2026w46"}, ""},
		{"forwarded with body device judged on end user", endUserLine{Fwd: true, IPSrc: "body", UASrc: "body", IPC: "manual"}, "manual"},
	}
	for _, c := range cases {
		u := c.u
		assert.Equal(t, c.want, blockReason(&u, l), c.name)
	}
	assert.Equal(t, "", blockReason(nil, l))
	assert.Equal(t, "", blockReason(&endUserLine{IPC: "manual"}, nil))
}

func TestEnforcesIsOffByDefaultAndHonoursBreaker(t *testing.T) {
	d := writeIVTDir(t, true)
	l := loadIVTLists(d)
	assert.False(t, l.enforces([]string{"pub-1234abcd"}), "no ivt-enforce.json: monitor")

	require.NoError(t, os.WriteFile(filepath.Join(d, "ivt-enforce.json"), []byte(`{"mode":"monitor","labels":["pub-1234abcd"]}`), 0644))
	assert.False(t, loadIVTLists(d).enforces([]string{"pub-1234abcd"}), "global switch in monitor")

	require.NoError(t, os.WriteFile(filepath.Join(d, "ivt-enforce.json"), []byte(`{"mode":"enforce","labels":["pub-1234abcd"]}`), 0644))
	l = loadIVTLists(d)
	assert.True(t, l.enforces([]string{"other", "pub-1234abcd"}))
	assert.False(t, l.enforces([]string{"other"}), "placement not switched on")

	require.NoError(t, os.WriteFile(filepath.Join(d, "ivt-breaker.json"), []byte(`{"labels":["pub-1234abcd"]}`), 0644))
	assert.False(t, loadIVTLists(d).enforces([]string{"pub-1234abcd"}), "breaker tripped")

	require.NoError(t, os.WriteFile(filepath.Join(d, "ivt-enforce.json"), []byte(`{not json`), 0644))
	assert.False(t, loadIVTLists(d).enforces([]string{"pub-1234abcd"}), "unreadable switch: monitor")
}

// processOnly runs entrypoint + processed and returns the processed result
// plus the merged module context, for asserting on rejection.
func processOnly(t *testing.T, m Module, xff string, req *openrtb2.BidRequest) (hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload], *hookstage.ModuleContext) {
	t.Helper()
	httpReq := httptest.NewRequest("POST", "/openrtb2/auction", strings.NewReader("{}"))
	httpReq.Header.Set("X-Forwarded-For", xff)
	entry, err := m.HandleEntrypointHook(context.Background(), hookstage.ModuleInvocationContext{},
		hookstage.EntrypointPayload{Request: httpReq, Body: []byte("{}")})
	require.NoError(t, err)
	mc := entry.ModuleContext
	req.ID = "a1"
	req.Imp = []openrtb2.Imp{impWithBidders("imp1", "pub-1234abcd", "thrad")}
	proc, err := m.HandleProcessedAuctionHook(context.Background(), hookstage.ModuleInvocationContext{ModuleContext: mc},
		hookstage.ProcessedAuctionRequestPayload{Request: &openrtb_ext.RequestWrapper{BidRequest: req}})
	require.NoError(t, err)
	mc.SetAll(proc.ModuleContext.GetAll())
	return proc, mc
}

func TestMonitorModeLogsShadowVerdictButNeverRejects(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), writeIVTDir(t, true))
	proc, mc := processOnly(t, m, "192.0.2.250", &openrtb2.BidRequest{Device: &openrtb2.Device{IP: "192.0.2.250", UA: chromeUA}})
	assert.False(t, proc.Reject)
	_, err := m.HandleAuctionResponseHook(context.Background(), hookstage.ModuleInvocationContext{ModuleContext: mc},
		hookstage.AuctionResponsePayload{BidResponse: &openrtb2.BidResponse{}})
	require.NoError(t, err)
	u := readLines(t, m.cfg.LogDir, "activity")[0]["u"].(map[string]interface{})
	assert.Equal(t, "manual", u["blk"])
	assert.Nil(t, u["enf"])
}

func TestEnforceRejectsBeforeBiddersAndStillLogs(t *testing.T) {
	d := writeIVTDir(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(d, "ivt-enforce.json"), []byte(`{"mode":"enforce","labels":["pub-1234abcd"]}`), 0644))
	m := buildIVTModule(t, t.TempDir(), d)

	proc, mc := processOnly(t, m, "192.0.2.250", &openrtb2.BidRequest{Device: &openrtb2.Device{IP: "192.0.2.250", UA: chromeUA}})
	require.True(t, proc.Reject)
	assert.Equal(t, SuspectedIVT, proc.NbrCode)
	// PBS runs auction_response after a reject (endpoints/openrtb2 rejectAuctionRequest)
	_, err := m.HandleAuctionResponseHook(context.Background(), hookstage.ModuleInvocationContext{ModuleContext: mc},
		hookstage.AuctionResponsePayload{BidResponse: &openrtb2.BidResponse{}})
	require.NoError(t, err)
	u := readLines(t, m.cfg.LogDir, "activity")[0]["u"].(map[string]interface{})
	assert.Equal(t, "manual", u["blk"])
	assert.Equal(t, true, u["enf"])

	// a clean user on the same enforced placement is served
	proc, _ = processOnly(t, m, homeUser, &openrtb2.BidRequest{Device: &openrtb2.Device{IP: homeUser, UA: chromeUA}})
	assert.False(t, proc.Reject)

	// unverifiable forwarded traffic is never rejected, even on the incident list
	proc, _ = processOnly(t, m, forwarder, &openrtb2.BidRequest{Device: &openrtb2.Device{IP: "192.0.2.250", UA: "undici"}})
	assert.False(t, proc.Reject)
}

func TestConfirmedBehaviourHashIsBlocked(t *testing.T) {
	d := writeIVTDir(t, true)
	iph := loadIVTLists(d).hash(netip.MustParseAddr(dcUser).String())
	require.NoError(t, os.WriteFile(filepath.Join(d, "confirmed.txt"), []byte("2026w46:"+iph+"\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(d, "ivt-enforce.json"), []byte(`{"mode":"enforce","labels":["pub-1234abcd"]}`), 0644))
	m := buildIVTModule(t, t.TempDir(), d)
	proc, _ := processOnly(t, m, dcUser, &openrtb2.BidRequest{Device: &openrtb2.Device{IP: dcUser, UA: chromeUA}})
	assert.True(t, proc.Reject)
	proc, _ = processOnly(t, m, "198.51.100.8", &openrtb2.BidRequest{Device: &openrtb2.Device{IP: "198.51.100.8", UA: chromeUA}})
	assert.False(t, proc.Reject, "another data-centre user on probation is served")
}

func TestAuctionLabels(t *testing.T) {
	imps := map[string]impActivity{"1": {StoredImpID: "pub-a"}, "2": {StoredImpID: ""}}
	assert.ElementsMatch(t, []string{"pub-a", "site:x.example"}, auctionLabels(imps, &endUserLine{Dom: "x.example"}))
	assert.ElementsMatch(t, []string{"app:com.y"}, auctionLabels(map[string]impActivity{}, &endUserLine{Bundle: "com.y"}))
}
