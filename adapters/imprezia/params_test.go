package imprezia

// This file actually intends to test static/bidder-params/imprezia.json
//
// These also validate the format of the external API: request.imp[i].ext.prebid.bidder.imprezia

import (
	"encoding/json"
	"testing"

	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// TestValidParams makes sure the imprezia schema accepts every imp.ext
// shape we actually send in production — including, critically, the
// Stored-Imp-only static shape (siteId/placementId, no request/response).
// That last case is the direct regression test for a real production
// incident (2026-08-22, learnrithm): an earlier version of this schema
// marked request/response "required", so PBS's static validation rejected
// the WHOLE imp — not just Imprezia's bid — on every auction fired before
// an assistant reply exists (i.e. before the bundle has anything to put in
// those two fields). See imprezia.go's package doc and
// openrtb_ext/imp_imprezia.go for the full writeup. Do not re-add
// "required" to static/bidder-params/imprezia.json.
func TestValidParams(t *testing.T) {
	validator, err := openrtb_ext.NewBidderParamsValidator("../../static/bidder-params")
	if err != nil {
		t.Fatalf("Failed to fetch the json-schemas. %v", err)
	}

	for _, validParam := range validParams {
		if err := validator.Validate(openrtb_ext.BidderImprezia, json.RawMessage(validParam)); err != nil {
			t.Errorf("Schema rejected imprezia params: %s\n%v", validParam, err)
		}
	}
}

// TestInvalidParams makes sure the imprezia schema still rejects params of
// the wrong shape — the fix here was removing a "required" list, not
// removing type checking.
func TestInvalidParams(t *testing.T) {
	validator, err := openrtb_ext.NewBidderParamsValidator("../../static/bidder-params")
	if err != nil {
		t.Fatalf("Failed to fetch the json-schemas. %v", err)
	}

	for _, invalidParam := range invalidParams {
		if err := validator.Validate(openrtb_ext.BidderImprezia, json.RawMessage(invalidParam)); err == nil {
			t.Errorf("Schema allowed unexpected params: %s", invalidParam)
		}
	}
}

var validParams = []string{
	// Full shape: static half (from the Stored Imp) merged with the
	// dynamic half (from the bundle) — the normal steady-state request
	// once an assistant reply exists. Includes timestamp/deviceContext,
	// added 2026-08-23 alongside request/response/sessionId.
	`{"request":"What are some good running shoes?","response":"I recommend cushioned neutral shoes for beginners.","timestamp":"2026-08-23T17:41:57.000Z","deviceContext":{"deviceType":"mobile","viewportWidth":390,"viewportHeight":844},"userId":"u1","sessionId":"s1","siteId":"cbc68717-3d85-4d55-9a29-e7a1a22ab4ef","placementId":"sayhola-chat-main","maxCards":1,"bidPrice":1.5}`,
	// Same steady-state shape but missing timestamp/deviceContext — must
	// still validate cleanly (2026-08-23 regression case, same incident
	// class as the request/response case below): MakeRequests, not the
	// schema, decides whether to skip.
	`{"request":"What are some good running shoes?","response":"I recommend cushioned neutral shoes for beginners.","siteId":"cbc68717-3d85-4d55-9a29-e7a1a22ab4ef","placementId":"sayhola-chat-main"}`,
	// Stored-Imp-only static shape — no request/response yet. THE
	// regression case: this is exactly what PBS validates on every
	// auction fired before an assistant reply exists, and it must pass.
	`{"siteId":"cbc68717-3d85-4d55-9a29-e7a1a22ab4ef","placementId":"sayhola-chat-main"}`,
	// Bundle-only dynamic shape — no siteId/placementId set (a publisher
	// that hasn't had a surface/placement registered yet, or one that
	// deliberately leaves them unset per the package doc's "optional"
	// note).
	`{"request":"hello","response":"hi there"}`,
	// Empty object — no static or dynamic fields at all. Must still
	// validate cleanly; MakeRequests is what decides whether to skip.
	`{}`,
}

var invalidParams = []string{
	// Wrong type for a dynamic field.
	`{"request":123,"response":"hi"}`,
	// Wrong type for a static field.
	`{"siteId":42}`,
	// Malformed deviceContext — wrong type for a nested field.
	`{"deviceContext":{"deviceType":"mobile","viewportWidth":"wide"}}`,
	// deviceContext.deviceType outside the enum.
	`{"deviceContext":{"deviceType":"smart-fridge"}}`,
	// Wrong type for maxCards.
	`{"maxCards":"one"}`,
	// maxCards below the schema's minimum.
	`{"maxCards":0}`,
	// Malformed JSON.
	`{"request":"hello"`,
}
