package adsense

// Package adsense implements a Prebid Server adapter for Google AdSense
// (ca-pub-5613991199249080) — a SYNTHETIC bidder, unlike every other
// adapter in this fork (Adform, Thrad, Imprezia). Full design rationale:
// docs/integration/adsense-integration-plan.md (tpc-org/docs).
//
// ── Why synthetic ───────────────────────────────────────────────────────
//
// adsbygoogle.js runs its own client-side ad request against Google's ad
// servers and privately decides what to show and what it's worth — there
// is no synchronous API where PBS can ask "what's your bid for this
// impression" and get a number back in time for an OpenRTB auction. So
// this adapter does not call Google at all. It always bids a fixed,
// server-configured CPM (extra_info.bidPrice, or a per-imp override) with
// a constant creative — the same "no price in the bid response" pattern
// Gravity/Imprezia use, but for a different underlying reason: those two
// call a real endpoint that just doesn't return a price; this one makes
// no real per-auction call to Google whatsoever.
//
// ── The loopback trick ───────────────────────────────────────────────────
//
// PBS's Bidder interface only lets an adapter contribute a bid by
// returning RequestData that the exchange actually fetches over HTTP and
// hands to MakeBids — an empty RequestData slice means zero bids, there's
// no "just synthesize a bid" hook. cfg.Endpoint here is configured to
// PBS's own loopback status endpoint (http://127.0.0.1:8000/status in
// this deployment's pbs.yaml — same host/process, not the public
// pbs.tpcsrv.com domain, which would needlessly route through the ALB/
// Global Accelerator). MakeBids ignores the response body entirely and
// unconditionally synthesizes the bid — confirmed safe by reading
// exchange/bidder.go: MakeBids receives the raw *adapters.ResponseData
// with zero framework-level validation of its shape, so an arbitrary
// small plain-text 200 body works fine. See the plan doc's Phase 1
// findings for the full trace; a live self-loopback test against a
// running PBS instance is still recommended before this carries real
// traffic (verified by reading code, not by executing it).
//
// ── Dual banner/native support ────────────────────────────────────────────
//
// The creative is Google's single responsive ad unit
// (data-ad-format="auto", data-full-width-responsive="true") — it fills
// whatever container it's given, so the same constant markup is returned
// for both banner and native imps. Banner: standard adm + W/H from the
// imp. Native: PBS does not require every declared native asset
// (including optional ones) to be present in the response, and passes a
// custom `ext` block through untouched — confirmed by reading
// exchange/bidder_validate_bids.go and exchange/bidder.go, and already
// proven in production by Imprezia's adapter, which does exactly this
// (conditional assets + a custom ext.imprezia block). So the native adm
// here populates only the one asset the sayhola native stored imp
// actually marks required (id 0, title) with a throwaway placeholder
// never shown to a real user, and carries the real creative in a custom
// `ext.tpc.adsense.html` field that only prebid-deployments' render
// pipeline reads — it must construct real DOM script/ins elements from
// that field (not innerHTML, which never executes <script> tags — a
// browser behavior confirmed in the plan doc's Phase 1, not a PBS
// concern).
//
// A single BidRequest CAN contain both a banner and a native imp in the
// same auction (e.g. a page loading both tpc-hola-banner and the sayhola
// native placement at once) if this bidder is configured on both stored
// imps — so, unlike Imprezia's adapter (which only ever sees a single
// imp per request in practice and takes a `request.Imp[0]` shortcut
// accordingly), MakeBids here looks up the specific imp for each
// RequestData/response pair by ID via RequestData.ImpIDs rather than
// assuming index 0.
//
// ── No auth, no dynamic per-auction params ──────────────────────────────
//
// Nothing here calls a real Google endpoint, so there's no API key/auth
// to configure. client/slot ID are constants baked into this file (the
// same AdSense ad unit is reused for every publisher and placement); the
// only per-imp override is an optional bidPrice, mirroring Imprezia's
// pattern. Unlike Thrad (userId) and Imprezia (request/response), there
// is no dynamic per-auction field the client bundle needs to forward —
// tpcBidAdapter.js's imp() converter needs no adsense-specific block.

import (
	"encoding/json"
	"fmt"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// ── Creative ────────────────────────────────────────────────────────────

// adCreativeHTML is Google's AdSense responsive ad-unit snippet, served
// verbatim for every publisher and every ad slot — see the package doc.
// Fixed per docs/integration/adsense-integration-plan.md; not templated
// per publisher because it's one shared account/ad-unit today.
const adCreativeHTML = `<script async src="https://pagead2.googlesyndication.com/pagead/js/adsbygoogle.js?client=ca-pub-5613991199249080" crossorigin="anonymous"></script>
<ins class="adsbygoogle" style="display:block" data-ad-client="ca-pub-5613991199249080" data-ad-slot="2544117831" data-ad-format="auto" data-full-width-responsive="true"></ins>
<script>(adsbygoogle = window.adsbygoogle || []).push({});</script>`

// adCreativeID is a fixed crid — there's only ever one creative, so no
// per-response ID is meaningful the way Imprezia's impressionUuid is.
const adCreativeID = "adsense-responsive"

// ── Config types ────────────────────────────────────────────────────────

type extraInfo struct {
	BidPrice float64 `json:"bidPrice"`
}

// ── Adapter ─────────────────────────────────────────────────────────────

type adapter struct {
	endpoint string
	info     extraInfo
}

func Builder(bidderName openrtb_ext.BidderName, cfg config.Adapter, server config.Server) (adapters.Bidder, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("adsense: endpoint must be configured — expected a local loopback " +
			"health-check URL (e.g. http://127.0.0.1:8000/status), never a real Google endpoint; " +
			"see docs/integration/adsense-integration-plan.md")
	}

	a := &adapter{endpoint: cfg.Endpoint}

	if cfg.ExtraAdapterInfo != "" {
		if err := json.Unmarshal([]byte(cfg.ExtraAdapterInfo), &a.info); err != nil {
			return nil, fmt.Errorf("adsense: unable to parse extra_info: %w", err)
		}
	}
	if a.info.BidPrice <= 0 {
		a.info.BidPrice = 1.0
	}

	return a, nil
}

// MakeRequests builds one loopback RequestData per biddable imp (banner
// or native only — AdSense's creative isn't declared for any other media
// type). No body, no auth: the call exists only to satisfy PBS's adapter
// interface (see package doc); MakeBids ignores everything about the
// response except that it succeeded.
func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var requests []*adapters.RequestData
	var errs []error

	for i := range request.Imp {
		imp := request.Imp[i]

		if imp.Banner == nil && imp.Native == nil {
			// Nothing this bidder can fill (e.g. video/audio-only imp) —
			// soft skip, not an error.
			continue
		}

		// Validate imp.ext shape even though nothing in it is required —
		// same defensive convention as every other adapter in this fork
		// (malformed ext for THIS imp shouldn't take down the others).
		var bidderExt adapters.ExtImpBidder
		if err := json.Unmarshal(imp.Ext, &bidderExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("adsense: invalid imp.ext for imp %s: %s", imp.ID, err),
			})
			continue
		}
		var impExt openrtb_ext.ExtImpAdsense
		if len(bidderExt.Bidder) > 0 {
			if err := json.Unmarshal(bidderExt.Bidder, &impExt); err != nil {
				errs = append(errs, &errortypes.BadInput{
					Message: fmt.Sprintf("adsense: invalid imp.ext.bidder for imp %s: %s", imp.ID, err),
				})
				continue
			}
		}

		requests = append(requests, &adapters.RequestData{
			Method: "GET",
			Uri:    a.endpoint,
			ImpIDs: []string{imp.ID},
		})
	}

	return requests, errs
}

// MakeBids ignores the loopback response body entirely (see package doc)
// and unconditionally synthesizes a fixed-price bid for whichever imp
// this RequestData/response pair corresponds to, shaped as banner or
// native to match that imp.
func (a *adapter) MakeBids(request *openrtb2.BidRequest, requestData *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The loopback call itself failed (e.g. PBS's own /status is
		// somehow unreachable) — this carries no real bid signal either
		// way, so treat it as a clean no-fill rather than a hard adapter
		// error.
		return nil, nil
	}

	if len(requestData.ImpIDs) != 1 {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("adsense: expected exactly 1 imp ID on requestData, got %d", len(requestData.ImpIDs)),
		}}
	}
	impID := requestData.ImpIDs[0]

	var imp *openrtb2.Imp
	for i := range request.Imp {
		if request.Imp[i].ID == impID {
			imp = &request.Imp[i]
			break
		}
	}
	if imp == nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("adsense: imp %s not found in request", impID),
		}}
	}

	bidPrice := a.info.BidPrice
	var bidderExt adapters.ExtImpBidder
	var impExt openrtb_ext.ExtImpAdsense
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
		AdM:   adCreativeHTML,
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
	nativeAdm, err := buildNativeAdm()
	if err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("adsense: failed to build native adm: %s", err),
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
// declared Format — same convention as reading a banner imp elsewhere in
// this fork.
func bannerSize(banner *openrtb2.Banner) (w, h int64) {
	if banner.W != nil && banner.H != nil {
		return *banner.W, *banner.H
	}
	if len(banner.Format) > 0 {
		return banner.Format[0].W, banner.Format[0].H
	}
	return 0, 0
}

// buildNativeAdm constructs a minimal-but-valid OpenRTB Native 1.2
// response: only the one asset the sayhola native stored imp actually
// marks required (id 0, title) is populated, with a throwaway placeholder
// never shown to a real user — PBS's own native validation doesn't
// require optional assets to be present (see package doc). The real
// creative rides in ext.tpc.adsense.html, which only our own client-side
// render pipeline reads.
func buildNativeAdm() (string, error) {
	type nativeTitle struct {
		Text string `json:"text"`
	}
	type nativeAsset struct {
		ID    int          `json:"id"`
		Title *nativeTitle `json:"title,omitempty"`
	}
	type adsenseExt struct {
		HTML string `json:"html"`
	}
	type tpcExt struct {
		Adsense adsenseExt `json:"adsense"`
	}
	type nativeExt struct {
		Tpc tpcExt `json:"tpc"`
	}
	type nativeAdmWrapper struct {
		Ver    string        `json:"ver"`
		Assets []nativeAsset `json:"assets"`
		Ext    nativeExt     `json:"ext"`
	}

	adm := nativeAdmWrapper{
		Ver:    "1.2",
		Assets: []nativeAsset{{ID: 0, Title: &nativeTitle{Text: "Advertisement"}}},
		Ext: nativeExt{Tpc: tpcExt{Adsense: adsenseExt{
			HTML: adCreativeHTML,
		}}},
	}

	b, err := json.Marshal(adm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
