package tpctest

// Package tpctest implements a deterministic, SYNTHETIC PBS bidder used
// only to validate a server-side integration partner's end-to-end
// pipeline (auction call -> bid parsing -> creative render/tracker
// firing) — never a real demand source. Full design rationale:
// docs/integration/test-bidder-plan.md (tpc-org/docs).
//
// ── Activation ────────────────────────────────────────────────────────
//
// Requires BOTH of:
//   - request-level "test": 1  (standard OpenRTB field, already used
//     elsewhere in this codebase — see exchange/exchange.go's
//     getDebugInfo call)
//   - imp[].ext.prebid.bidder.tpctest present on that imp
//
// MakeRequests refuses to produce any bid requests at all unless the
// incoming request is OpenRTB test-flagged. This is a hard kill-switch,
// independent of whether an imp carries a tpctest block — a stray copy
// of that block into a real Stored Imp can never fire on real traffic.
//
// A caller does not need a dedicated Stored Imp to use this: PBS
// resolves a Stored Imp reference by JSON-merge-patching the caller's
// own imp onto the stored imp (endpoints/openrtb2/auction.go), so a
// partner can add ext.prebid.bidder.tpctest inline on top of their own
// real storedrequest.id in their own request.
//
// ── The loopback trick ───────────────────────────────────────────────
//
// Same technique as adapters/adsense/adsense.go: cfg.Endpoint is PBS's
// own loopback status endpoint (never a real external endpoint).
// MakeBids ignores the response body entirely and unconditionally
// synthesizes a fixed bid.
//
// ── Full-asset native, unlike AdSense ────────────────────────────────
//
// AdSense's adapter only ever populates the one native asset PBS
// requires, since its real creative rides in a custom ext blob. This
// bidder's whole purpose is exercising a partner's asset-mapping code,
// so MakeBids parses the imp's declared native asset spec
// (imp.native.request) and returns placeholder content for every
// declared asset (title/img/data), not just the required one.
//
// ── No auth, no dynamic per-auction params ──────────────────────────
//
// Nothing here calls any real external endpoint, so there is no
// API key/auth to configure and no policy exposure (unlike the
// still-unresolved AdSense program-policy question — see
// docs/integration/test-bidder-plan.md). The only per-imp override is
// an optional bidPrice, mirroring the AdSense/Imprezia convention.
//
// Fork-only per FORK_NOTES.md — do not include in upstream PBS PRs.

import (
	"encoding/json"
	"fmt"

	"github.com/prebid/openrtb/v20/openrtb2"

	nativeRequests "github.com/prebid/openrtb/v20/native1/request"
	nativeResponses "github.com/prebid/openrtb/v20/native1/response"

	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// ── Creative ────────────────────────────────────────────────────────

const testCreativeLabel = "TEST AD"

// testImageURL is a placeholder image — no real asset is fetched or
// served; any 1x1-safe, always-reachable image would do here, this one
// simply lives on infrastructure we already control.
const testImageURL = "https://s3.tpcsrv.com/prod/tpctest-placeholder.png"

// adCreativeID is a fixed crid — there's only ever one (synthetic)
// creative, so no per-response ID is meaningful.
const adCreativeID = "tpctest-deterministic"

const defaultBidPrice = 5.0

// ── Config types ────────────────────────────────────────────────────

type extraInfo struct {
	BidPrice float64 `json:"bidPrice"`
}

// ── Adapter ─────────────────────────────────────────────────────────

type adapter struct {
	endpoint string
	info     extraInfo
}

func Builder(_ openrtb_ext.BidderName, cfg config.Adapter, _ config.Server) (adapters.Bidder, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("tpctest: endpoint must be configured — expected a local loopback " +
			"health-check URL (e.g. http://127.0.0.1:8000/status), never a real external endpoint; " +
			"see docs/integration/test-bidder-plan.md")
	}

	a := &adapter{endpoint: cfg.Endpoint}

	if cfg.ExtraAdapterInfo != "" {
		if err := json.Unmarshal([]byte(cfg.ExtraAdapterInfo), &a.info); err != nil {
			return nil, fmt.Errorf("tpctest: unable to parse extra_info: %w", err)
		}
	}
	if a.info.BidPrice <= 0 {
		a.info.BidPrice = defaultBidPrice
	}

	return a, nil
}

// MakeRequests refuses to produce any bid requests at all unless the
// request is OpenRTB test-flagged — see the package doc's "Activation"
// section. This is checked before anything else.
func (a *adapter) MakeRequests(bidRequest *openrtb2.BidRequest, _ *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	if bidRequest.Test != 1 {
		return nil, nil
	}

	var requests []*adapters.RequestData

	for i := range bidRequest.Imp {
		imp := bidRequest.Imp[i]

		if imp.Banner == nil && imp.Native == nil {
			// Nothing this bidder can fill (e.g. video/audio-only imp) —
			// soft skip, not an error.
			continue
		}

		requests = append(requests, &adapters.RequestData{
			Method: "GET",
			Uri:    a.endpoint,
			ImpIDs: []string{imp.ID},
		})
	}

	return requests, nil
}

// MakeBids ignores the loopback response body entirely (see package
// doc) and unconditionally synthesizes a fixed-price bid for whichever
// imp this RequestData/response pair corresponds to.
func (a *adapter) MakeBids(bidRequest *openrtb2.BidRequest, requestData *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The loopback call itself failed — carries no real bid signal
		// either way, so treat it as a clean no-fill.
		return nil, nil
	}

	if len(requestData.ImpIDs) != 1 {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("tpctest: expected exactly 1 imp ID on requestData, got %d", len(requestData.ImpIDs)),
		}}
	}
	impID := requestData.ImpIDs[0]

	var imp *openrtb2.Imp
	for i := range bidRequest.Imp {
		if bidRequest.Imp[i].ID == impID {
			imp = &bidRequest.Imp[i]
			break
		}
	}
	if imp == nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("tpctest: imp %s not found in request", impID),
		}}
	}

	bidPrice := a.info.BidPrice
	var bidderExt adapters.ExtImpBidder
	var impExt openrtb_ext.ExtImpTpcTest
	if err := json.Unmarshal(imp.Ext, &bidderExt); err == nil {
		if err := json.Unmarshal(bidderExt.Bidder, &impExt); err == nil {
			if impExt.BidPrice > 0 {
				bidPrice = impExt.BidPrice
			}
		}
	}

	switch {
	case imp.Banner != nil:
		return bannerBidderResponse(imp, bidPrice)
	case imp.Native != nil:
		return nativeBidderResponse(imp, bidPrice)
	default:
		// Shouldn't happen — MakeRequests already filtered to
		// banner/native imps only — but no bid is the safe fallback.
		return nil, nil
	}
}

func bannerBidderResponse(imp *openrtb2.Imp, bidPrice float64) (*adapters.BidderResponse, []error) {
	w, h := bannerSize(imp.Banner)

	ortbBid := openrtb2.Bid{
		ID:    imp.ID,
		ImpID: imp.ID,
		Price: bidPrice,
		AdM:   bannerCreativeHTML(w, h),
		W:     w,
		H:     h,
		CrID:  adCreativeID,
		MType: openrtb2.MarkupBanner,
	}

	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(1)
	bidderResponse.Bids = append(bidderResponse.Bids, &adapters.TypedBid{
		Bid:     &ortbBid,
		BidType: openrtb_ext.BidTypeBanner,
	})
	bidderResponse.Currency = "USD"

	return bidderResponse, nil
}

func nativeBidderResponse(imp *openrtb2.Imp, bidPrice float64) (*adapters.BidderResponse, []error) {
	nativeAdm, err := buildNativeAdm(imp.Native.Request)
	if err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("tpctest: failed to build native adm: %s", err),
		}}
	}

	ortbBid := openrtb2.Bid{
		ID:    imp.ID,
		ImpID: imp.ID,
		Price: bidPrice,
		AdM:   nativeAdm,
		CrID:  adCreativeID,
		MType: openrtb2.MarkupNative,
	}

	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(1)
	bidderResponse.Bids = append(bidderResponse.Bids, &adapters.TypedBid{
		Bid:     &ortbBid,
		BidType: openrtb_ext.BidTypeNative,
	})
	bidderResponse.Currency = "USD"

	return bidderResponse, nil
}

// bannerSize prefers explicit Banner.W/H, falling back to the first
// declared Format — same convention as adapters/adsense/adsense.go.
func bannerSize(banner *openrtb2.Banner) (w, h int64) {
	if banner.W != nil && banner.H != nil {
		return *banner.W, *banner.H
	}
	if len(banner.Format) > 0 {
		return banner.Format[0].W, banner.Format[0].H
	}
	return 0, 0
}

// bannerCreativeHTML is a fully self-contained, inert placeholder — no
// script tags, no external resources beyond nothing at all, so there is
// zero network dependency for a partner's render test to fail on.
func bannerCreativeHTML(w, h int64) string {
	return fmt.Sprintf(
		`<div style="width:%dpx;height:%dpx;display:flex;align-items:center;justify-content:center;`+
			`background:#f0f0f0;border:2px dashed #999;font-family:sans-serif;color:#666;">%s</div>`,
		w, h, testCreativeLabel,
	)
}

// buildNativeAdm parses the imp's declared native asset spec and
// returns a fully populated Native 1.2 response — every declared asset
// gets placeholder content, not just the one PBS itself requires (see
// package doc's "Full-asset native" section). Assets whose type this
// bidder doesn't recognize (e.g. video) are skipped rather than guessed
// at.
func buildNativeAdm(nativeRequestJSON string) (string, error) {
	var req nativeRequests.Request
	if err := json.Unmarshal([]byte(nativeRequestJSON), &req); err != nil {
		return "", fmt.Errorf("invalid native.request: %w", err)
	}

	assets := make([]nativeResponses.Asset, 0, len(req.Assets))
	for _, reqAsset := range req.Assets {
		id := reqAsset.ID
		respAsset := nativeResponses.Asset{ID: &id}

		switch {
		case reqAsset.Title != nil:
			respAsset.Title = &nativeResponses.Title{Text: testTitleText(reqAsset.Title.Len)}
		case reqAsset.Img != nil:
			w, h := imageSize(reqAsset.Img)
			respAsset.Img = &nativeResponses.Image{URL: testImageURL, W: w, H: h}
		case reqAsset.Data != nil:
			respAsset.Data = &nativeResponses.Data{Value: testCreativeLabel + " — data"}
		default:
			continue
		}

		assets = append(assets, respAsset)
	}

	adm := nativeResponses.Response{
		Ver:    "1.2",
		Assets: assets,
		Link:   nativeResponses.Link{URL: "https://tpcsrv.com/tpctest"},
	}

	b, err := json.Marshal(adm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func testTitleText(maxLen int64) string {
	text := testCreativeLabel + " — Title"
	if maxLen > 0 && int64(len(text)) > maxLen {
		return text[:maxLen]
	}
	return text
}

func imageSize(img *nativeRequests.Image) (w, h int64) {
	w, h = img.W, img.H
	if w == 0 {
		w = img.WMin
	}
	if h == 0 {
		h = img.HMin
	}
	if w == 0 {
		w = 300
	}
	if h == 0 {
		h = 250
	}
	return w, h
}
