package tpctest

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
	bidder, err := Builder(openrtb_ext.BidderTpcTest, config.Adapter{
		Endpoint:         "http://127.0.0.1:8000/status",
		ExtraAdapterInfo: `{"bidPrice":5.0}`,
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	return bidder
}

func tpctestImp(t *testing.T, id string, ext map[string]interface{}, banner *openrtb2.Banner, native *openrtb2.Native) openrtb2.Imp {
	t.Helper()
	bidderExt, err := json.Marshal(map[string]interface{}{"bidder": ext})
	if err != nil {
		t.Fatalf("failed to marshal imp ext: %v", err)
	}
	return openrtb2.Imp{ID: id, Ext: bidderExt, Banner: banner, Native: native}
}

func ptr(v int64) *int64 { return &v }

const sampleNativeRequest = `{"ver":"1.2","assets":[` +
	`{"id":0,"required":1,"title":{"len":80}},` +
	`{"id":1,"required":0,"img":{"type":3,"wmin":300,"hmin":250}},` +
	`{"id":2,"required":0,"data":{"type":2,"len":100}}` +
	`]}`

func TestBuilderRequiresEndpoint(t *testing.T) {
	_, err := Builder(openrtb_ext.BidderTpcTest, config.Adapter{
		ExtraAdapterInfo: `{"bidPrice":5.0}`,
	}, config.Server{})
	if err == nil {
		t.Fatal("expected Builder to error when endpoint is missing")
	}
}

func TestBuilderDefaultsBidPrice(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderTpcTest, config.Adapter{
		Endpoint: "http://127.0.0.1:8000/status",
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	a := bidder.(*adapter)
	if a.info.BidPrice != defaultBidPrice {
		t.Errorf("BidPrice = %v, want %v default", a.info.BidPrice, defaultBidPrice)
	}
}

// TestMakeRequestsRequiresTestFlag is the regression guard for the
// package doc's hard safety gate: this bidder must never produce any
// bid requests at all unless the incoming request is OpenRTB
// test-flagged, independent of whether bidder.tpctest is present on the
// imp. This is what makes it safe for a stray tpctest block to end up
// on a real Stored Imp.
func TestMakeRequestsRequiresTestFlag(t *testing.T) {
	bidder := testBuilder(t)
	imp := tpctestImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)

	for _, testVal := range []int8{0, 2} {
		request := &openrtb2.BidRequest{ID: "req-1", Test: testVal, Imp: []openrtb2.Imp{imp}}
		reqData, errs := bidder.MakeRequests(request, nil)
		if len(errs) != 0 {
			t.Fatalf("test=%d: unexpected errors: %v", testVal, errs)
		}
		if len(reqData) != 0 {
			t.Fatalf("test=%d: expected 0 requests when request.Test != 1, got %d", testVal, len(reqData))
		}
	}
}

func TestMakeRequestsBuildsOneLoopbackRequestPerImpWhenTestFlagged(t *testing.T) {
	bidder := testBuilder(t)
	bannerImp := tpctestImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	nativeImp := tpctestImp(t, "native-1", nil, nil, &openrtb2.Native{Request: sampleNativeRequest})
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{bannerImp, nativeImp}}

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
}

func TestMakeRequestsSkipsNonBannerNonNativeImps(t *testing.T) {
	bidder := testBuilder(t)
	imp := tpctestImp(t, "1", nil, nil, nil)
	imp.Video = &openrtb2.Video{}
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{imp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
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

func TestMakeBidsBanner(t *testing.T) {
	bidder := testBuilder(t)
	imp := tpctestImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{imp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "banner-1", http.StatusOK)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
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
	if bid.Bid.Price != 5.0 {
		t.Errorf("Price = %v, want 5.0 default", bid.Bid.Price)
	}
}

// TestMakeBidsNativeFillsEveryDeclaredAsset is the core behavioral
// difference from adapters/adsense: every asset the request declares —
// not just the one PBS requires — must come back populated, so a
// partner's full asset-mapping code gets exercised.
func TestMakeBidsNativeFillsEveryDeclaredAsset(t *testing.T) {
	bidder := testBuilder(t)
	imp := tpctestImp(t, "native-1", nil, nil, &openrtb2.Native{Request: sampleNativeRequest})
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{imp}}

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
		Ver  string `json:"ver"`
		Link struct {
			URL string `json:"url"`
		} `json:"link"`
		Assets []struct {
			ID    *int64 `json:"id"`
			Title *struct {
				Text string `json:"text"`
			} `json:"title"`
			Img *struct {
				URL string `json:"url"`
				W   int64  `json:"w"`
				H   int64  `json:"h"`
			} `json:"img"`
			Data *struct {
				Value string `json:"value"`
			} `json:"data"`
		} `json:"assets"`
	}
	if err := json.Unmarshal([]byte(bid.Bid.AdM), &adm); err != nil {
		t.Fatalf("failed to unmarshal native adm: %v", err)
	}
	if adm.Link.URL == "" {
		t.Error("expected a non-empty top-level link.url")
	}
	if len(adm.Assets) != 3 {
		t.Fatalf("expected 3 assets (title, img, data), got %d: %+v", len(adm.Assets), adm.Assets)
	}
	for _, a := range adm.Assets {
		if a.ID == nil {
			t.Fatalf("asset missing id: %+v", a)
		}
		switch *a.ID {
		case 0:
			if a.Title == nil || a.Title.Text == "" {
				t.Errorf("asset 0: expected populated title, got %+v", a)
			}
		case 1:
			if a.Img == nil || a.Img.URL == "" || a.Img.W == 0 || a.Img.H == 0 {
				t.Errorf("asset 1: expected populated img with size, got %+v", a)
			}
		case 2:
			if a.Data == nil || a.Data.Value == "" {
				t.Errorf("asset 2: expected populated data, got %+v", a)
			}
		default:
			t.Errorf("unexpected asset id %d", *a.ID)
		}
	}
}

// TestMakeBidsNativeTitleRespectsMaxLen confirms the placeholder title
// never exceeds the request's declared max length — a real partner's
// asset-mapping code may reject or truncate an over-length title, and
// this bidder shouldn't manufacture that failure itself.
func TestMakeBidsNativeTitleRespectsMaxLen(t *testing.T) {
	bidder := testBuilder(t)
	shortTitleRequest := `{"ver":"1.2","assets":[{"id":0,"required":1,"title":{"len":5}}]}`
	imp := tpctestImp(t, "native-1", nil, nil, &openrtb2.Native{Request: shortTitleRequest})
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{imp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "native-1", http.StatusOK)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var adm struct {
		Assets []struct {
			Title *struct {
				Text string `json:"text"`
			} `json:"title"`
		} `json:"assets"`
	}
	if err := json.Unmarshal([]byte(bidderResponse.Bids[0].Bid.AdM), &adm); err != nil {
		t.Fatalf("failed to unmarshal native adm: %v", err)
	}
	if len(adm.Assets) != 1 || adm.Assets[0].Title == nil {
		t.Fatalf("expected one title asset, got %+v", adm.Assets)
	}
	if len(adm.Assets[0].Title.Text) > 5 {
		t.Errorf("title text %q exceeds declared max len 5", adm.Assets[0].Title.Text)
	}
}

func TestMakeBidsBidPriceOverride(t *testing.T) {
	bidder := testBuilder(t)
	imp := tpctestImp(t, "banner-1", map[string]interface{}{"bidPrice": 14.0}, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{imp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "banner-1", http.StatusOK)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if bidderResponse.Bids[0].Bid.Price != 14.0 {
		t.Errorf("Price = %v, want 14.0 override", bidderResponse.Bids[0].Bid.Price)
	}
}

func TestMakeBidsMultiImpRequestPicksCorrectImp(t *testing.T) {
	bidder := testBuilder(t)
	bannerImp := tpctestImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	nativeImp := tpctestImp(t, "native-1", nil, nil, &openrtb2.Native{Request: sampleNativeRequest})
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{nativeImp, bannerImp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "banner-1", http.StatusOK)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if bidderResponse.Bids[0].BidType != openrtb_ext.BidTypeBanner {
		t.Errorf("BidType = %v, want banner (resolved by ID, not position)", bidderResponse.Bids[0].BidType)
	}
}

func TestMakeBidsNonSuccessStatusIsCleanNoFill(t *testing.T) {
	bidder := testBuilder(t)
	imp := tpctestImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{imp}}

	bidderResponse, errs := doMakeBids(t, bidder, request, "banner-1", http.StatusServiceUnavailable)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
	if bidderResponse != nil {
		t.Fatalf("expected nil bidderResponse, got %+v", bidderResponse)
	}
}

func TestMakeBidsImpNotFoundErrors(t *testing.T) {
	bidder := testBuilder(t)
	imp := tpctestImp(t, "banner-1", nil, &openrtb2.Banner{W: ptr(300), H: ptr(250)}, nil)
	request := &openrtb2.BidRequest{ID: "req-1", Test: 1, Imp: []openrtb2.Imp{imp}}

	_, errs := doMakeBids(t, bidder, request, "does-not-exist", http.StatusOK)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
}
