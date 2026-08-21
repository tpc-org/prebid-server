package imprezia

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

func testBuilder(t *testing.T) adapters.Bidder {
	t.Helper()
	bidder, err := Builder(openrtb_ext.BidderImprezia, config.Adapter{
		Endpoint: "https://api.imprezia.ai/v1/ads/chat",
		ExtraAdapterInfo: `{"apiKey":"test-prod-key","sandboxApiKey":"test-sandbox-key",` +
			`"sandboxEndpoint":"https://api-sandbox.imprezia.ai/v1/ads/chat","bidPrice":1.0}`,
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	return bidder
}

func impreziaImp(t *testing.T, ext map[string]interface{}) openrtb2.Imp {
	t.Helper()
	bidderExt, err := json.Marshal(map[string]interface{}{"bidder": ext})
	if err != nil {
		t.Fatalf("failed to marshal imp ext: %v", err)
	}
	return openrtb2.Imp{ID: "1", Ext: bidderExt}
}

func baseImpreziaExt(overrides map[string]interface{}) map[string]interface{} {
	ext := map[string]interface{}{
		"request":  "What are some good running shoes?",
		"response": "I recommend cushioned neutral shoes for beginners.",
	}
	for k, v := range overrides {
		ext[k] = v
	}
	return ext
}

// TestBuilderRequiresAPIKey mirrors Gravity/Thrad's Builder validation —
// extra_info must contain a non-empty apiKey, or PBS's boot-time
// exchange.BuildAdapters() would fail startup entirely (see the package
// doc's "no price field" section and docs/CLAUDE.md's m152.yaml incident).
func TestBuilderRequiresAPIKey(t *testing.T) {
	_, err := Builder(openrtb_ext.BidderImprezia, config.Adapter{
		Endpoint:         "https://api.imprezia.ai/v1/ads/chat",
		ExtraAdapterInfo: `{"bidPrice":1.0}`,
	}, config.Server{})
	if err == nil {
		t.Fatal("expected Builder to error when apiKey is missing")
	}
}

// TestBuilderDefaultsBidPrice confirms the same 1.0 fallback default as
// Gravity when extra_info.bidPrice is omitted or zero.
func TestBuilderDefaultsBidPrice(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderImprezia, config.Adapter{
		Endpoint:         "https://api.imprezia.ai/v1/ads/chat",
		ExtraAdapterInfo: `{"apiKey":"test-key"}`,
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	a := bidder.(*adapter)
	if a.info.BidPrice != 1.0 {
		t.Errorf("BidPrice = %v, want 1.0 default", a.info.BidPrice)
	}
}

// TestMakeRequestsRequiresRequestAndResponse locks in the confirmed API
// contract: request/response are the ONLY two fields Imprezia's real API
// requires. A missing value must skip the imp with a BadInput error, not
// send a doomed-to-400 request.
func TestMakeRequestsRequiresRequestAndResponse(t *testing.T) {
	bidder := testBuilder(t)

	tests := []struct {
		name      string
		overrides map[string]interface{}
	}{
		{"missing request", map[string]interface{}{"request": ""}},
		{"missing response", map[string]interface{}{"response": ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			imp := impreziaImp(t, baseImpreziaExt(tc.overrides))
			request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

			reqData, errs := bidder.MakeRequests(request, nil)
			if len(errs) != 1 {
				t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
			}
			if len(reqData) != 0 {
				t.Fatalf("expected 0 requests sent, got %d", len(reqData))
			}
		})
	}
}

// TestMakeRequestsOmitsOptionalFieldsWhenNotProvided is the regression guard
// for the schema decision in static/bidder-params/imprezia.json: userId,
// sessionId, siteId, and placementId are genuinely optional in the wire
// body, unlike Gravity where the equivalent fields are required. Getting
// this wrong (declaring them required) is exactly the incident class
// documented in docs/runbooks/gravity-reactivation.md — PBS would reject
// the whole multi-bidder imp, not just Imprezia's own bid.
func TestMakeRequestsOmitsOptionalFieldsWhenNotProvided(t *testing.T) {
	bidder := testBuilder(t)
	imp := impreziaImp(t, baseImpreziaExt(nil))
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(reqData) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqData))
	}

	var body map[string]interface{}
	if err := json.Unmarshal(reqData[0].Body, &body); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	for _, field := range []string{"userId", "sessionId", "siteId", "placementId"} {
		if _, present := body[field]; present {
			t.Errorf("body[%q] should be omitted when not provided, got %v", field, body[field])
		}
	}
	if got := body["maxCards"]; got != float64(defaultMaxCards) {
		t.Errorf(`body["maxCards"] = %v, want default %d`, got, defaultMaxCards)
	}
}

// TestMakeRequestsSendsAllFieldsWhenProvided confirms siteId/placementId/
// userId/sessionId all pass through correctly when present.
func TestMakeRequestsSendsAllFieldsWhenProvided(t *testing.T) {
	bidder := testBuilder(t)
	imp := impreziaImp(t, baseImpreziaExt(map[string]interface{}{
		"userId":      "user-1",
		"sessionId":   "session-1",
		"siteId":      "cbc68717-3d85-4d55-9a29-e7a1a22ab4ef",
		"placementId": "chat_followup",
	}))
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(reqData[0].Body, &body); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	want := map[string]string{
		"userId":      "user-1",
		"sessionId":   "session-1",
		"siteId":      "cbc68717-3d85-4d55-9a29-e7a1a22ab4ef",
		"placementId": "chat_followup",
	}
	for field, expected := range want {
		if got := body[field]; got != expected {
			t.Errorf("body[%q] = %v, want %q", field, got, expected)
		}
	}
	if got := reqData[0].Headers.Get("X-API-Key"); got != "test-prod-key" {
		t.Errorf("X-API-Key header = %q, want production key", got)
	}
}

// TestMakeRequestsUsesSandboxEndpointAndKeyWhenTest1 is the one genuinely
// new behavior vs. Gravity/Thrad: Imprezia switches HOST, not just key,
// between environments.
func TestMakeRequestsUsesSandboxEndpointAndKeyWhenTest1(t *testing.T) {
	bidder := testBuilder(t)
	imp := impreziaImp(t, baseImpreziaExt(nil))
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{imp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(reqData) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqData))
	}
	if got := reqData[0].Uri; got != "https://api-sandbox.imprezia.ai/v1/ads/chat" {
		t.Errorf("Uri = %q, want sandbox endpoint", got)
	}
	if got := reqData[0].Headers.Get("X-API-Key"); got != "test-sandbox-key" {
		t.Errorf("X-API-Key header = %q, want sandbox key", got)
	}
}

// TestMakeBidsNoContentReturnsNoBid mirrors Gravity/Thrad's 204 handling.
func TestMakeBidsNoContentReturnsNoBid(t *testing.T) {
	bidder := testBuilder(t)
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{{ID: "1"}}}
	response := &adapters.ResponseData{StatusCode: http.StatusNoContent}

	bidderResponse, errs := bidder.MakeBids(request, nil, response)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if bidderResponse != nil {
		t.Errorf("expected nil bidderResponse for 204, got %v", bidderResponse)
	}
}

// TestMakeBidsNoAdReturnsNoBid confirms a 200 response with no "ad" key is
// treated as a clean no-fill, not an error. Not empirically observed live
// (sandbox always returned a fixed house-ad creative in testing) but
// handled defensively — see imprezia.go's package doc.
func TestMakeBidsNoAdReturnsNoBid(t *testing.T) {
	bidder := testBuilder(t)
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{{ID: "1"}}}
	body := `{"requestId":"req_1","siteId":"fcdcaadc-5f24-4253-ada2-43777bb6b078","placementId":null,"ad":null}`
	response := &adapters.ResponseData{StatusCode: http.StatusOK, Body: []byte(body)}

	bidderResponse, errs := bidder.MakeBids(request, nil, response)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if bidderResponse != nil {
		t.Errorf("expected nil bidderResponse for a null ad, got %v", bidderResponse)
	}
}

// TestMakeBidsParsesRealSandboxResponse uses a REAL response body captured
// live against api-sandbox.imprezia.ai/v1/ads/chat on 2026-08-21 (request
// ID/tokens/signed-URL params redacted/shortened for readability, but the
// structure and field names are verbatim — see imprezia.go's package doc
// for how this differs from Imprezia's own (wrong) documented SDK-response
// claim). Confirmed live: maxCards has no effect on this shape, and the
// same fixed house-ad creative was returned across multiple distinct
// queries — this is normal sandbox behavior, not a bug in this test.
func TestMakeBidsParsesRealSandboxResponse(t *testing.T) {
	bidder := testBuilder(t)
	imp := impreziaImp(t, baseImpreziaExt(nil))
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	body := `{
		"requestId": "req_1787339023476_avrt16nqa",
		"siteId": "fcdcaadc-5f24-4253-ada2-43777bb6b078",
		"placementId": null,
		"ad": {
			"creative": {
				"brandName": "Imprezia",
				"title": "Developers. Earn money with your AI app.",
				"description": "Run ads like this, and get paid. Your AI flows are untouched. Earn like the big guys at: imprezia.ai",
				"cta": "Sponsored",
				"imageUrl": "https://storage.googleapis.com/imprezia-sandbox/keVMxd9oWPuLWrcq?X-Goog-Signature=abc123"
			},
			"clickUrl": "https://go-sandbox.imprezia.ai/go/bEh4zJa_c80YTv2ZjPT2vlJXYKAT_7Q35n9IamNeyWU",
			"trackers": {
				"impression": ["https://r-sandbox.imprezia.net/tp/impression-token-abc"],
				"mrc50": ["https://r-sandbox.imprezia.net/tp/mrc50-token-abc"]
			},
			"impression": {
				"impressionUuid": "c7395975-7454-4992-a828-012aec0136ac",
				"beaconToken": {"token": "v5neUNfggWQ9IXX-rX4x0ygVeBxRGyHYiVr6Sy4vHZU", "issuedAt": 1787339026629, "kid": "20260427-01"},
				"servedAt": "2026-08-21T19:03:46.636Z",
				"publisherId": "dc01fce8-2239-4070-b50a-0441aef10f8f"
			}
		}
	}`
	response := &adapters.ResponseData{StatusCode: http.StatusOK, Body: []byte(body)}

	bidderResponse, errs := bidder.MakeBids(request, nil, response)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if bidderResponse == nil || len(bidderResponse.Bids) != 1 {
		t.Fatalf("expected 1 bid, got %v", bidderResponse)
	}

	bid := bidderResponse.Bids[0]
	if bid.BidType != openrtb_ext.BidTypeNative {
		t.Errorf("BidType = %v, want native", bid.BidType)
	}
	if bid.Bid.Price != 1.0 {
		t.Errorf("Price = %v, want fallback bidPrice 1.0", bid.Bid.Price)
	}
	if len(bid.Bid.ADomain) != 1 || bid.Bid.ADomain[0] != "go-sandbox.imprezia.ai" {
		t.Errorf("ADomain = %v, want [go-sandbox.imprezia.ai] (from clickUrl's host)", bid.Bid.ADomain)
	}
	if bid.Bid.CrID != "c7395975-7454-4992-a828-012aec0136ac" {
		t.Errorf("CrID = %q, want the impressionUuid", bid.Bid.CrID)
	}

	var adm map[string]interface{}
	if err := json.Unmarshal([]byte(bid.Bid.AdM), &adm); err != nil {
		t.Fatalf("failed to unmarshal adm: %v", err)
	}
	if link, ok := adm["link"].(map[string]interface{}); !ok || link["url"] != "https://go-sandbox.imprezia.ai/go/bEh4zJa_c80YTv2ZjPT2vlJXYKAT_7Q35n9IamNeyWU" {
		t.Errorf("adm.link.url = %v, want ad.clickUrl", adm["link"])
	}
	assets, ok := adm["assets"].([]interface{})
	if !ok || len(assets) == 0 {
		t.Fatalf("expected native assets in adm, got %v", adm["assets"])
	}
	title := assets[0].(map[string]interface{})["title"].(map[string]interface{})["text"]
	if title != "Developers. Earn money with your AI app." {
		t.Errorf("assets[0].title.text = %v, want the creative title", title)
	}
	impTrackers, ok := adm["imptrackers"].([]interface{})
	if !ok || len(impTrackers) != 2 {
		t.Fatalf("expected 2 imptrackers (impression + mrc50), got %v", adm["imptrackers"])
	}
	if impTrackers[0] != "https://r-sandbox.imprezia.net/tp/impression-token-abc" {
		t.Errorf("imptrackers[0] = %v, want the impression tracker", impTrackers[0])
	}
	if impTrackers[1] != "https://r-sandbox.imprezia.net/tp/mrc50-token-abc" {
		t.Errorf("imptrackers[1] = %v, want the mrc50 tracker", impTrackers[1])
	}
}

// TestMakeBidsBadRequestReturnsError mirrors Gravity's 400 handling.
func TestMakeBidsBadRequestReturnsError(t *testing.T) {
	bidder := testBuilder(t)
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{{ID: "1"}}}
	response := &adapters.ResponseData{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"bad"}`)}

	_, errs := bidder.MakeBids(request, nil, response)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
}
