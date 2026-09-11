package adsense

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
	bidder, err := Builder(openrtb_ext.BidderAdsense, config.Adapter{
		Endpoint:         "http://127.0.0.1:8000/status",
		ExtraAdapterInfo: `{"bidPrice":1.0}`,
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	return bidder
}

func adsenseImp(t *testing.T, id string, ext map[string]interface{}, banner *openrtb2.Banner, native *openrtb2.Native) openrtb2.Imp {
	t.Helper()
	bidderExt, err := json.Marshal(map[string]interface{}{"bidder": ext})
	if err != nil {
		t.Fatalf("failed to marshal imp ext: %v", err)
	}
	return openrtb2.Imp{ID: id, Ext: bidderExt, Banner: banner, Native: native}
}

func ptr(v int64) *int64 { return &v }

// TestBuilderRequiresEndpoint locks in that this MUST be configured to a
// local loopback URL — an empty endpoint would mean MakeRequests has
// nowhere to point RequestData.Uri at all.
func TestBuilderRequiresEndpoint(t *testing.T) {
	_, err := Builder(openrtb_ext.BidderAdsense, config.Adapter{
		ExtraAdapterInfo: `{"bidPrice":1.0}`,
	}, config.Server{})
	if err == nil {
		t.Fatal("expected Builder to error when endpoint is missing")
	}
}

// TestBuilderDefaultsBidPrice mirrors Gravity/Imprezia's $1.00 fallback
// when extra_info.bidPrice is omitted or zero.
func TestBuilderDefaultsBidPrice(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderAdsense, config.Adapter{
		Endpoint: "http://127.0.0.1:8000/status",
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	a := bidder.(*adapter)
	if a.info.BidPrice != 1.0 {
		t.Errorf("BidPrice = %v, want 1.0 default", a.info.BidPrice)
	}
}

// TestMakeRequestsSkipsNonBannerNonNativeImps confirms an imp with
// neither Banner nor Native (e.g. video-only) is silently skipped, not
// erred — there's nothing this bidder can fill it with.
func TestMakeRequestsSkipsNonBannerNonNativeImps(t *testing.T) {
	bidder := testBuilder(t)
	imp := adsenseImp(t, "1", nil, nil, nil)
	imp.Video = &openrtb2.Video{}
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(reqData) != 0 {
		t.Fatalf("expected 0 requests, got %d", len(reqData))
	}
}

// TestMakeRequestsBuildsOneLoopbackRequestPerImp confirms both a banner
// and a native imp in the same auction each get their own loopback
// RequestData, tagged with that imp's own ID (not a shortcut assuming a
// single imp per request, unlike Imprezia — see the package doc).
func TestMakeRequestsBuildsOneLoopbackRequestPerImp(t *testing.T) {
	bidder := testBuilder(t)
	bannerImp := adsenseImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	nativeImp := adsenseImp(t, "native-1", nil, nil, &openrtb2.Native{})
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{bannerImp, nativeImp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(reqData) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqData))
	}
	for _, rd := range reqData {
		if rd.Uri != "http://127.0.0.1:8000/status" {
			t.Errorf("Uri = %q, want loopback status endpoint", rd.Uri)
		}
		if rd.Method != "GET" {
			t.Errorf("Method = %q, want GET", rd.Method)
		}
		if len(rd.ImpIDs) != 1 {
			t.Errorf("expected exactly 1 ImpID, got %d", len(rd.ImpIDs))
		}
	}
	if reqData[0].ImpIDs[0] != "banner-1" || reqData[1].ImpIDs[0] != "native-1" {
		t.Errorf("ImpIDs = [%v, %v], want [banner-1, native-1]", reqData[0].ImpIDs, reqData[1].ImpIDs)
	}
}

// TestMakeRequestsRejectsMalformedExt confirms a broken imp.ext.bidder
// for one imp doesn't silently produce a bogus request.
func TestMakeRequestsRejectsMalformedExt(t *testing.T) {
	bidder := testBuilder(t)
	imp := openrtb2.Imp{ID: "1", Ext: json.RawMessage(`{"bidder":{"bidPrice":"not-a-number"}}`), Banner: &openrtb2.Banner{W: ptr(300), H: ptr(250)}}
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if len(reqData) != 0 {
		t.Fatalf("expected 0 requests, got %d", len(reqData))
	}
}

func doMakeBids(t *testing.T, bidder adapters.Bidder, request *openrtb2.BidRequest, impID string, statusCode int) (*adapters.BidderResponse, []error) {
	t.Helper()
	requestData := &adapters.RequestData{Method: "GET", Uri: "http://127.0.0.1:8000/status", ImpIDs: []string{impID}}
	response := &adapters.ResponseData{StatusCode: statusCode, Body: []byte("ok")}
	return bidder.MakeBids(request, requestData, response)
}

// TestMakeBidsBanner confirms the banner bid picks up the imp's own
// W/H, the fixed creative, MType banner, and the global bidPrice.
func TestMakeBidsBanner(t *testing.T) {
	bidder := testBuilder(t)
	imp := adsenseImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "banner-1", http.StatusOK)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(bidderResponse.Bids) != 1 {
		t.Fatalf("expected 1 bid, got %d", len(bidderResponse.Bids))
	}
	bid := bidderResponse.Bids[0]
	if bid.BidType != openrtb_ext.BidTypeBanner {
		t.Errorf("BidType = %v, want banner", bid.BidType)
	}
	if bid.Bid.MType != openrtb2.MarkupBanner {
		t.Errorf("MType = %v, want MarkupBanner", bid.Bid.MType)
	}
	if bid.Bid.W != 300 || bid.Bid.H != 250 {
		t.Errorf("W/H = %d/%d, want 300/250", bid.Bid.W, bid.Bid.H)
	}
	if bid.Bid.Price != 1.0 {
		t.Errorf("Price = %v, want 1.0 default", bid.Bid.Price)
	}
	if bid.Bid.AdM != adCreativeHTML {
		t.Errorf("AdM does not match the fixed AdSense creative")
	}
}

// TestMakeBidsBannerFallsBackToFormat confirms a Format-only banner
// (no explicit W/H) is still sized correctly.
func TestMakeBidsBannerFallsBackToFormat(t *testing.T) {
	bidder := testBuilder(t)
	imp := adsenseImp(t, "banner-1", nil, &openrtb2.Banner{Format: []openrtb2.Format{{W: 728, H: 90}}}, nil)
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "banner-1", http.StatusOK)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	bid := bidderResponse.Bids[0]
	if bid.Bid.W != 728 || bid.Bid.H != 90 {
		t.Errorf("W/H = %d/%d, want 728/90 from Format[0]", bid.Bid.W, bid.Bid.H)
	}
}

// TestMakeBidsNative confirms the native bid populates only the one
// asset the request actually requires (id 0, title, with a throwaway
// placeholder), MType native, and carries the real creative in
// ext.tpc.adsense.html — see the package doc and
// docs/integration/adsense-integration-plan.md's Phase 1 findings for
// why this shape is safe against PBS's own native validation.
func TestMakeBidsNative(t *testing.T) {
	bidder := testBuilder(t)
	imp := adsenseImp(t, "native-1", nil, nil, &openrtb2.Native{})
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "native-1", http.StatusOK)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	bid := bidderResponse.Bids[0]
	if bid.BidType != openrtb_ext.BidTypeNative {
		t.Errorf("BidType = %v, want native", bid.BidType)
	}
	if bid.Bid.MType != openrtb2.MarkupNative {
		t.Errorf("MType = %v, want MarkupNative", bid.Bid.MType)
	}

	var adm struct {
		Ver    string `json:"ver"`
		Assets []struct {
			ID    int `json:"id"`
			Title *struct {
				Text string `json:"text"`
			} `json:"title"`
		} `json:"assets"`
		Ext struct {
			Tpc struct {
				Adsense struct {
					HTML string `json:"html"`
				} `json:"adsense"`
			} `json:"tpc"`
		} `json:"ext"`
	}
	if err := json.Unmarshal([]byte(bid.Bid.AdM), &adm); err != nil {
		t.Fatalf("failed to unmarshal native adm: %v", err)
	}
	if len(adm.Assets) != 1 || adm.Assets[0].ID != 0 || adm.Assets[0].Title == nil {
		t.Fatalf("expected exactly one title asset (id 0), got %+v", adm.Assets)
	}
	if adm.Ext.Tpc.Adsense.HTML != adCreativeHTML {
		t.Errorf("ext.tpc.adsense.html does not match the fixed AdSense creative")
	}
}

// TestMakeBidsBidPriceOverride confirms a per-imp bidPrice wins over the
// global extra_info default — same convention as Imprezia.
func TestMakeBidsBidPriceOverride(t *testing.T) {
	bidder := testBuilder(t)
	imp := adsenseImp(t, "banner-1", map[string]interface{}{"bidPrice": 14.0}, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "banner-1", http.StatusOK)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if bidderResponse.Bids[0].Bid.Price != 14.0 {
		t.Errorf("Price = %v, want 14.0 override", bidderResponse.Bids[0].Bid.Price)
	}
}

// TestMakeBidsMultiImpRequestPicksCorrectImp is the regression guard for
// the package doc's claim that this adapter does NOT take Imprezia's
// request.Imp[0] shortcut: a request batching both a banner and a native
// imp must resolve each RequestData/response pair to the matching imp by
// ID, not by position.
func TestMakeBidsMultiImpRequestPicksCorrectImp(t *testing.T) {
	bidder := testBuilder(t)
	bannerImp := adsenseImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	nativeImp := adsenseImp(t, "native-1", nil, nil, &openrtb2.Native{})
	// Native listed first in request.Imp, but we ask MakeBids to resolve
	// the SECOND request.Imp entry's RequestData (banner-1) — if the
	// adapter took an Imp[0] shortcut this would wrongly build a native
	// bid instead.
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{nativeImp, bannerImp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "banner-1", http.StatusOK)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if bidderResponse.Bids[0].BidType != openrtb_ext.BidTypeBanner {
		t.Errorf("BidType = %v, want banner (resolved by ID, not position)", bidderResponse.Bids[0].BidType)
	}
}

// TestMakeBidsNonSuccessStatusIsCleanNoFill confirms a failed loopback
// call (e.g. PBS's own /status somehow unreachable) degrades to no bid
// rather than a hard adapter error — it carries no real bid signal
// either way.
func TestMakeBidsNonSuccessStatusIsCleanNoFill(t *testing.T) {
	bidder := testBuilder(t)
	imp := adsenseImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "banner-1", http.StatusServiceUnavailable)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
	if bidderResponse != nil {
		t.Fatalf("expected nil bidderResponse, got %+v", bidderResponse)
	}
}

// TestMakeBidsImpNotFoundErrors confirms a RequestData whose ImpIDs
// don't match anything in the (possibly filtered) request is a hard
// error, not a silent no-fill — that would indicate a real bug, not an
// expected no-fill condition.
func TestMakeBidsImpNotFoundErrors(t *testing.T) {
	bidder := testBuilder(t)
	imp := adsenseImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	_, errs := doMakeBids(t, bidder, request, "does-not-exist", http.StatusOK)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
}
