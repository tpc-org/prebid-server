package paramfanout

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildRequest(t *testing.T, impExt map[string]json.RawMessage) *openrtb_ext.RequestWrapper {
	t.Helper()
	extBytes, err := json.Marshal(map[string]interface{}{
		"prebid": map[string]interface{}{"bidder": impExt},
	})
	require.NoError(t, err)
	req := &openrtb2.BidRequest{
		Imp: []openrtb2.Imp{{ID: "1", Ext: extBytes}},
	}
	rw := &openrtb_ext.RequestWrapper{BidRequest: req}
	require.NoError(t, rw.RebuildRequest())
	return rw
}

func bidderExt(t *testing.T, rw *openrtb_ext.RequestWrapper, bidder string) map[string]interface{} {
	t.Helper()
	require.NoError(t, rw.RebuildRequest())
	var ext struct {
		Prebid struct {
			Bidder map[string]json.RawMessage `json:"bidder"`
		} `json:"prebid"`
	}
	require.NoError(t, json.Unmarshal(rw.Imp[0].Ext, &ext))
	raw, ok := ext.Prebid.Bidder[bidder]
	if !ok {
		return nil
	}
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func run(t *testing.T, m Module, rw *openrtb_ext.RequestWrapper) hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload] {
	t.Helper()
	payload := hookstage.ProcessedAuctionRequestPayload{Request: rw}
	result, err := m.HandleProcessedAuctionHook(context.Background(), hookstage.ModuleInvocationContext{}, payload)
	require.NoError(t, err)
	for _, mut := range result.ChangeSet.Mutations() {
		p, err := mut.Apply(payload)
		require.NoError(t, err)
		payload = p
	}
	require.NoError(t, payload.Request.RebuildRequest())
	return result
}

func TestFanOutThradAndImprezia(t *testing.T) {
	rw := buildRequest(t, map[string]json.RawMessage{
		"tpc": mustJSON(t, map[string]interface{}{
			"userId":    "user-1",
			"sessionId": "session-1",
			"messages": []map[string]string{
				{"role": "user", "content": "I'm looking for new shoes"},
			},
		}),
		"thrad": mustJSON(t, map[string]interface{}{
			"publisherId": "sayhola",
			"requestType": "opener",
		}),
		"imprezia": mustJSON(t, map[string]interface{}{
			"siteId":      "cbc68717-3d85-4d55-9a29-e7a1a22ab4ef",
			"placementId": "sayhola-chat-main",
		}),
	})

	m := Module{enabled: true}
	result := run(t, m, rw)
	assert.NotEmpty(t, result.ChangeSet.Mutations())

	thrad := bidderExt(t, rw, "thrad")
	assert.Equal(t, "user-1", thrad["userId"])
	assert.Equal(t, "sayhola", thrad["publisherId"], "static field must survive untouched")

	imprezia := bidderExt(t, rw, "imprezia")
	assert.Equal(t, "I'm looking for new shoes", imprezia["request"])
	assert.Equal(t, " ", imprezia["response"], "no assistant turn yet -> single-space placeholder, never empty")
	assert.Equal(t, "session-1", imprezia["sessionId"])
	assert.Equal(t, "cbc68717-3d85-4d55-9a29-e7a1a22ab4ef", imprezia["siteId"], "static field must survive untouched")
}

func TestFanOutImpreziaUsesRealAssistantReply(t *testing.T) {
	rw := buildRequest(t, map[string]json.RawMessage{
		"tpc": mustJSON(t, map[string]interface{}{
			"sessionId": "session-1",
			"messages": []map[string]string{
				{"role": "user", "content": "I'm looking for new shoes"},
				{"role": "assistant", "content": "Here are some options..."},
			},
		}),
		"imprezia": mustJSON(t, map[string]interface{}{"siteId": "s1"}),
	})

	run(t, Module{enabled: true}, rw)

	imprezia := bidderExt(t, rw, "imprezia")
	assert.Equal(t, "Here are some options...", imprezia["response"])
}

func TestNoOpWithoutGenericBlock(t *testing.T) {
	rw := buildRequest(t, map[string]json.RawMessage{
		"thrad": mustJSON(t, map[string]interface{}{"publisherId": "sayhola"}),
	})
	before := string(rw.Imp[0].Ext)

	result := run(t, Module{enabled: true}, rw)

	assert.Empty(t, result.ChangeSet.Mutations())
	assert.JSONEq(t, before, string(rw.Imp[0].Ext))
}

func TestNeverOverwritesAlreadyPopulatedFields(t *testing.T) {
	// Models exactly what the web bundle (tpcBidAdapter.js) already sends
	// today: fully-populated bidder-specific ext, built client-side,
	// alongside whatever it puts under the "tpc" key. This must be a
	// complete no-op, or turning this module on would change behavior for
	// every existing web auction.
	rw := buildRequest(t, map[string]json.RawMessage{
		"tpc": mustJSON(t, map[string]interface{}{
			"userId": "generic-user-id-should-not-be-used",
		}),
		"thrad": mustJSON(t, map[string]interface{}{
			"publisherId": "sayhola",
			"userId":      "already-set-by-web-bundle",
		}),
		"imprezia": mustJSON(t, map[string]interface{}{
			"siteId":    "s1",
			"request":   "already-set-by-web-bundle",
			"response":  "already-set-by-web-bundle",
			"sessionId": "already-set-by-web-bundle",
		}),
	})

	run(t, Module{enabled: true}, rw)

	thrad := bidderExt(t, rw, "thrad")
	assert.Equal(t, "already-set-by-web-bundle", thrad["userId"])

	imprezia := bidderExt(t, rw, "imprezia")
	assert.Equal(t, "already-set-by-web-bundle", imprezia["request"])
	assert.Equal(t, "already-set-by-web-bundle", imprezia["response"])
	assert.Equal(t, "already-set-by-web-bundle", imprezia["sessionId"])
}

func TestDisabledModuleIsNoOp(t *testing.T) {
	rw := buildRequest(t, map[string]json.RawMessage{
		"tpc":   mustJSON(t, map[string]interface{}{"userId": "user-1"}),
		"thrad": mustJSON(t, map[string]interface{}{"publisherId": "sayhola"}),
	})

	result := run(t, Module{enabled: false}, rw)

	assert.Empty(t, result.ChangeSet.Mutations())
	thrad := bidderExt(t, rw, "thrad")
	assert.Nil(t, thrad["userId"])
}

func mustJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
