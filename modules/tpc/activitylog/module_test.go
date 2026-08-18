package activitylog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/modules/moduledeps"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildModule(t *testing.T, logDir string, debugFlagFile string) Module {
	t.Helper()
	cfgMap := map[string]interface{}{"enabled": true, "log_dir": logDir}
	if debugFlagFile != "" {
		cfgMap["debug_flag_file"] = debugFlagFile
	}
	cfg, err := json.Marshal(cfgMap)
	require.NoError(t, err)

	built, err := Builder(cfg, moduledeps.ModuleDeps{})
	require.NoError(t, err)
	module, ok := built.(Module)
	require.True(t, ok)
	return module
}

// impWithBidders builds an openrtb2.Imp whose merged ext.prebid.bidder map
// has one entry per given bidder name (empty object values — content
// doesn't matter for these tests), plus a storedrequest.id, matching the
// shape PBS produces after the real stored-imp merge (see
// prebid-server/endpoints/openrtb2/auction.go's merge, and
// modules/tpc/profanityfilter's own imp-construction test helper).
func impWithBidders(id, storedImpID string, bidders ...string) openrtb2.Imp {
	bidderMap := map[string]interface{}{}
	for _, b := range bidders {
		bidderMap[b] = map[string]interface{}{"some": "param"}
	}
	extJSON, _ := json.Marshal(map[string]interface{}{
		"prebid": map[string]interface{}{
			"storedrequest": map[string]interface{}{"id": storedImpID},
			"bidder":        bidderMap,
		},
	})
	return openrtb2.Imp{ID: id, Ext: extJSON}
}

func processRequest(t *testing.T, module Module, imps []openrtb2.Imp) hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload] {
	t.Helper()
	payload := hookstage.ProcessedAuctionRequestPayload{
		Request: &openrtb_ext.RequestWrapper{
			BidRequest: &openrtb2.BidRequest{
				ID:     "test-auction",
				Imp:    imps,
				Device: &openrtb2.Device{IP: "203.0.113.5"},
			},
		},
	}
	result, err := module.HandleProcessedAuctionHook(context.Background(), hookstage.ModuleInvocationContext{}, payload)
	require.NoError(t, err)
	return result
}

func processResponse(t *testing.T, module Module, moduleCtx hookstage.ModuleContext, resp *openrtb2.BidResponse) {
	t.Helper()
	payload := hookstage.AuctionResponsePayload{BidResponse: resp}
	_, err := module.HandleAuctionResponseHook(context.Background(), hookstage.ModuleInvocationContext{ModuleContext: moduleCtx}, payload)
	require.NoError(t, err)
}

func seatBid(seat string, impIDs ...string) openrtb2.SeatBid {
	bids := make([]openrtb2.Bid, len(impIDs))
	for i, id := range impIDs {
		bids[i] = openrtb2.Bid{ID: "bid-" + id, ImpID: id, Price: 1.0}
	}
	return openrtb2.SeatBid{Seat: seat, Bid: bids}
}

func readLines(t *testing.T, dir, prefix string) []map[string]interface{} {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, prefix+".log-*"))
	require.NoError(t, err)
	if len(matches) == 0 {
		return nil
	}
	data, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	var lines []map[string]interface{}
	for _, raw := range splitNonEmptyLines(data) {
		var m map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &m))
		lines = append(lines, m)
	}
	return lines
}

func splitNonEmptyLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				out = append(out, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}

func TestFullAuctionWritesActivityLineWithRequestAndBidCounts(t *testing.T) {
	dir := t.TempDir()
	module := buildModule(t, dir, "")

	result := processRequest(t, module, []openrtb2.Imp{
		impWithBidders("imp1", "sayhola-9243e9b6", "thrad", "gravity"),
	})

	resp := &openrtb2.BidResponse{
		SeatBid: []openrtb2.SeatBid{
			seatBid("thrad", "imp1"),
			// gravity requested but returns zero bids -- no seatbid entry at all.
		},
	}
	processResponse(t, module, result.ModuleContext, resp)

	lines := readLines(t, dir, "activity")
	require.Len(t, lines, 1)
	imps := lines[0]["imps"].([]interface{})
	require.Len(t, imps, 1)
	imp := imps[0].(map[string]interface{})
	assert.Equal(t, "sayhola-9243e9b6", imp["stored_imp_id"])

	bidders := imp["bidders"].(map[string]interface{})
	thrad := bidders["thrad"].(map[string]interface{})
	assert.Equal(t, true, thrad["requested"])
	assert.Equal(t, float64(1), thrad["bids"])

	// The whole point: a bidder that was invoked but returned zero bids
	// must still show requested=true, bids=0 -- distinguishable from
	// "never invoked at all", which the earlier ad hoc Gravity
	// investigation this feature replaces couldn't get from seatbid alone.
	gravity := bidders["gravity"].(map[string]interface{})
	assert.Equal(t, true, gravity["requested"])
	assert.Equal(t, float64(0), gravity["bids"])
}

func TestTpcBidderKeyExcludedFromTallies(t *testing.T) {
	dir := t.TempDir()
	module := buildModule(t, dir, "")

	result := processRequest(t, module, []openrtb2.Imp{
		impWithBidders("imp1", "sayhola-60b4e117", "tpc", "adform"),
	})
	processResponse(t, module, result.ModuleContext, &openrtb2.BidResponse{
		SeatBid: []openrtb2.SeatBid{seatBid("adform", "imp1")},
	})

	lines := readLines(t, dir, "activity")
	require.Len(t, lines, 1)
	imp := lines[0]["imps"].([]interface{})[0].(map[string]interface{})
	bidders := imp["bidders"].(map[string]interface{})
	_, hasTpc := bidders["tpc"]
	assert.False(t, hasTpc, "tpc is the client wrapper bidder code, not a real demand partner")
	assert.Contains(t, bidders, "adform")
}

func TestMultiImpMultiBidderCorrelation(t *testing.T) {
	dir := t.TempDir()
	module := buildModule(t, dir, "")

	result := processRequest(t, module, []openrtb2.Imp{
		impWithBidders("imp1", "sayhola-60b4e117", "adform"),
		impWithBidders("imp2", "sayhola-9243e9b6", "thrad", "gravity"),
	})
	processResponse(t, module, result.ModuleContext, &openrtb2.BidResponse{
		SeatBid: []openrtb2.SeatBid{
			seatBid("adform", "imp1"),
			seatBid("thrad", "imp2", "imp2"), // 2 bids from thrad on the native imp
		},
	})

	lines := readLines(t, dir, "activity")
	require.Len(t, lines, 1)
	byStoredID := map[string]map[string]interface{}{}
	for _, raw := range lines[0]["imps"].([]interface{}) {
		imp := raw.(map[string]interface{})
		byStoredID[imp["stored_imp_id"].(string)] = imp
	}
	require.Contains(t, byStoredID, "sayhola-60b4e117")
	require.Contains(t, byStoredID, "sayhola-9243e9b6")

	nativeBidders := byStoredID["sayhola-9243e9b6"]["bidders"].(map[string]interface{})
	thrad := nativeBidders["thrad"].(map[string]interface{})
	assert.Equal(t, float64(2), thrad["bids"])
	gravity := nativeBidders["gravity"].(map[string]interface{})
	assert.Equal(t, float64(0), gravity["bids"])
}

func TestNoModuleContextIsCleanNoOp(t *testing.T) {
	// Simulates a request rejected by an earlier hook (e.g. profanityfilter)
	// before this module's own ProcessedAuctionRequest hook ran -- must not
	// panic or error, just skip writing anything.
	dir := t.TempDir()
	module := buildModule(t, dir, "")

	payload := hookstage.AuctionResponsePayload{BidResponse: &openrtb2.BidResponse{}}
	_, err := module.HandleAuctionResponseHook(context.Background(), hookstage.ModuleInvocationContext{}, payload)
	require.NoError(t, err)

	lines := readLines(t, dir, "activity")
	assert.Empty(t, lines)
}

func TestDisabledModuleWritesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg, err := json.Marshal(map[string]interface{}{"enabled": false, "log_dir": dir})
	require.NoError(t, err)
	built, err := Builder(cfg, moduledeps.ModuleDeps{})
	require.NoError(t, err)
	module := built.(Module)

	result := processRequest(t, module, []openrtb2.Imp{impWithBidders("imp1", "sayhola-60b4e117", "adform")})
	assert.Nil(t, result.ModuleContext)
	processResponse(t, module, result.ModuleContext, &openrtb2.BidResponse{})

	assert.Empty(t, readLines(t, dir, "activity"))
}

func TestDebugFlagAbsentNeverWritesDebugLog(t *testing.T) {
	dir := t.TempDir()
	module := buildModule(t, dir, filepath.Join(dir, "DEBUG_FULL_LOGGING")) // never created

	result := processRequest(t, module, []openrtb2.Imp{impWithBidders("imp1", "sayhola-9243e9b6", "thrad")})
	processResponse(t, module, result.ModuleContext, &openrtb2.BidResponse{SeatBid: []openrtb2.SeatBid{seatBid("thrad", "imp1")}})

	assert.NotEmpty(t, readLines(t, dir, "activity"))
	assert.Empty(t, readLines(t, dir, "debug"))
}

func TestDebugFlagFreshWritesDebugLogWithFullContent(t *testing.T) {
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "DEBUG_FULL_LOGGING")
	require.NoError(t, os.WriteFile(flagPath, []byte{}, 0644))
	module := buildModule(t, dir, flagPath)

	result := processRequest(t, module, []openrtb2.Imp{impWithBidders("imp1", "sayhola-9243e9b6", "thrad")})
	processResponse(t, module, result.ModuleContext, &openrtb2.BidResponse{SeatBid: []openrtb2.SeatBid{seatBid("thrad", "imp1")}})

	debugLines := readLines(t, dir, "debug")
	require.Len(t, debugLines, 1)
	assert.Equal(t, "203.0.113.5", debugLines[0]["device_ip"])
	imps := debugLines[0]["imps"].([]interface{})
	require.Len(t, imps, 1)
	imp := imps[0].(map[string]interface{})
	assert.Equal(t, "sayhola-9243e9b6", imp["stored_imp_id"])
	assert.Contains(t, imp["bidder_ext"].(map[string]interface{}), "thrad")
}

func TestDebugFlagExpiredIsTreatedAsOff(t *testing.T) {
	dir := t.TempDir()
	flagPath := filepath.Join(dir, "DEBUG_FULL_LOGGING")
	require.NoError(t, os.WriteFile(flagPath, []byte{}, 0644))
	oldTime := time.Now().Add(-5 * time.Hour) // older than the 4h default window
	require.NoError(t, os.Chtimes(flagPath, oldTime, oldTime))

	module := buildModule(t, dir, flagPath)
	result := processRequest(t, module, []openrtb2.Imp{impWithBidders("imp1", "sayhola-9243e9b6", "thrad")})
	processResponse(t, module, result.ModuleContext, &openrtb2.BidResponse{SeatBid: []openrtb2.SeatBid{seatBid("thrad", "imp1")}})

	assert.NotEmpty(t, readLines(t, dir, "activity"))
	assert.Empty(t, readLines(t, dir, "debug"), "flag file older than the debug window must be treated as inactive")
}

func TestImpWithoutStoredRequestIDIsOmittedFromOutput(t *testing.T) {
	dir := t.TempDir()
	module := buildModule(t, dir, "")

	result := processRequest(t, module, []openrtb2.Imp{impWithBidders("imp1", "", "thrad")})
	processResponse(t, module, result.ModuleContext, &openrtb2.BidResponse{})

	lines := readLines(t, dir, "activity")
	// No usable placement identifier -- nothing meaningful to report.
	if len(lines) == 1 {
		assert.Empty(t, lines[0]["imps"])
	}
}

func TestBuilderErrorsWhenEnabledWithoutLogDir(t *testing.T) {
	cfg, err := json.Marshal(map[string]interface{}{"enabled": true})
	require.NoError(t, err)
	_, err = Builder(cfg, moduledeps.ModuleDeps{})
	assert.Error(t, err)
}

func TestBuilderDisabledByDefault(t *testing.T) {
	built, err := Builder(json.RawMessage(`{}`), moduledeps.ModuleDeps{})
	require.NoError(t, err)
	module := built.(Module)
	assert.False(t, module.cfg.Enabled)
}
