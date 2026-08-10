package gravity

// Package gravity implements a Prebid Server adapter for Gravity (trygravity.ai).
//
// ── API key ──────────────────────────────────────────────────────────────────
//
// Configured in pbs-settings/pbs.yaml under adapters.gravity.extra_info:
//
//	adapters:
//	  gravity:
//	    endpoint: "https://server.trygravity.ai/api/v1/ad"
//	    extra_info: '{"apiKey":"KaQvh...","bidPrice":1.0}'
//
// bidPrice is the CPM (USD) reported to PBS — Gravity does not return a price
// in its response. Defaults to 1.0 if omitted.
//
// ── Test mode ────────────────────────────────────────────────────────────────
//
// When BidRequest.Test == 1 (set by Prebid.js when ?pbjs_debug=true), the
// adapter sets testAd: true in the Gravity request. Gravity returns test
// creatives and skips billing.
//
// ── Conversational context ───────────────────────────────────────────────────
//
// Gravity requires userId, sessionId, and placement params per auction.
// Static params (placement, placementId) live in the PBS Stored Imp.
// Dynamic params (userId, sessionId, messages) are injected per-auction
// via window.tpc.data → tpcBidAdapter → imp.ext.prebid.bidder.gravity.
//
// ── Hashed email (logged-in users) ────────────────────────────────────────────
//
// Added 2026-08-10. ExtImpGravity.HashedEmail (imp.ext.prebid.bidder.gravity.
// hashedEmail) forwards through as gravityUser.EmailHash — "email_hash" on
// the wire, per Gravity's real API, NOT "hashed_email" (a field-name bug
// existed here for a while: this field was defined but silently never
// worked, since Gravity's API accepts-and-ignores unrecognized user-object
// fields rather than rejecting them). Publisher-side responsibility to hash
// correctly (SHA-256 of email.strip().lower()) before it ever reaches us —
// see ExtImpGravity's doc comment. Thrad has no equivalent; their docs
// explicitly prohibit PII (including hashed email) in their userId field,
// so this is Gravity-only.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"text/template"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/macros"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// ── Config types ──────────────────────────────────────────────────────────────

type extraInfo struct {
	APIKey   string  `json:"apiKey"`
	BidPrice float64 `json:"bidPrice"`
}

// ── Gravity API types ─────────────────────────────────────────────────────────

type gravityRequest struct {
	Messages       []openrtb_ext.ExtImpGravityMsg `json:"messages,omitempty"`
	SessionID      string                         `json:"sessionId"`
	Placements     []gravityPlacement             `json:"placements"`
	User           gravityUser                    `json:"user"`
	ExcludedTopics []string                       `json:"excludedTopics,omitempty"`
	Relevancy      *float64                       `json:"relevancy,omitempty"`
	TestAd         bool                           `json:"testAd,omitempty"`
}

type gravityPlacement struct {
	Placement   string `json:"placement"`
	PlacementID string `json:"placement_id"`
}

type gravityUser struct {
	ID string `json:"id"`
	// EmailHash — field name confirmed against Gravity's own OpenAPI spec
	// (docs.trygravity.ai/api-reference/openapi.json, UserObject schema):
	// "email_hash", not "hashed_email". Getting this wrong is silent, not a
	// hard error — Gravity's API accepts unrecognized user-object fields and
	// stores them as generic request context rather than rejecting them, so
	// a wrong field name here would never surface as a bid failure, just as
	// hashed-email matching quietly never working. Must be SHA-256 of
	// email.strip().lower() (their normalization requirement) — hashing
	// happens publisher-side, before it ever reaches us (see
	// ExtImpGravity.HashedEmail's doc comment).
	EmailHash   string `json:"email_hash,omitempty"`
	HashedPhone string `json:"hashed_phone,omitempty"`
}

// gravityAd is one element of the Gravity API response array.
type gravityAd struct {
	AdText      string `json:"adText"`
	Title       string `json:"title"`
	BrandName   string `json:"brandName"`
	CTA         string `json:"cta"`
	URL         string `json:"url"`
	Favicon     string `json:"favicon"`
	ClickURL    string `json:"clickUrl"`
	ImpURL      string `json:"impUrl"`
	Placement   string `json:"placement"`
	PlacementID string `json:"placement_id"`
}

// ── Adapter ───────────────────────────────────────────────────────────────────

type adapter struct {
	endpoint *template.Template
	info     extraInfo
}

func Builder(bidderName openrtb_ext.BidderName, cfg config.Adapter, server config.Server) (adapters.Bidder, error) {
	tmpl, err := template.New("endpointTemplate").Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("gravity: unable to parse endpoint template: %w", err)
	}

	a := &adapter{endpoint: tmpl}

	if cfg.ExtraAdapterInfo != "" {
		if err := json.Unmarshal([]byte(cfg.ExtraAdapterInfo), &a.info); err != nil {
			return nil, fmt.Errorf("gravity: unable to parse extra_info: %w", err)
		}
	}
	if a.info.APIKey == "" {
		return nil, fmt.Errorf("gravity: extra_info must contain apiKey")
	}
	if a.info.BidPrice <= 0 {
		a.info.BidPrice = 1.0
	}

	return a, nil
}

// MakeRequests translates the OpenRTB auction into one Gravity API call per imp.
func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var requests []*adapters.RequestData
	var errs []error

	testAd := request.Test == 1

	for i := range request.Imp {
		imp := request.Imp[i]

		var bidderExt adapters.ExtImpBidder
		if err := json.Unmarshal(imp.Ext, &bidderExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("gravity: invalid imp.ext for imp %s: %s", imp.ID, err),
			})
			continue
		}

		var gravExt openrtb_ext.ExtImpGravity
		if err := json.Unmarshal(bidderExt.Bidder, &gravExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("gravity: invalid imp.ext.bidder for imp %s: %s", imp.ID, err),
			})
			continue
		}

		if gravExt.UserID == "" {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("gravity: userId is required for imp %s", imp.ID),
			})
			continue
		}
		if gravExt.SessionID == "" {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("gravity: sessionId is required for imp %s", imp.ID),
			})
			continue
		}
		if gravExt.Placement == "" {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("gravity: placement is required for imp %s", imp.ID),
			})
			continue
		}
		if gravExt.PlacementID == "" {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("gravity: placementId is required for imp %s", imp.ID),
			})
			continue
		}

		if len(gravExt.Messages) == 0 {
			// Gravity API requires messages (conversation context). Skip rather than
			// send a request that will always return 400.
			continue
		}

		gravReq := gravityRequest{
			Messages:  gravExt.Messages,
			SessionID: gravExt.SessionID,
			Placements: []gravityPlacement{
				{Placement: gravExt.Placement, PlacementID: gravExt.PlacementID},
			},
			User: gravityUser{
				ID:          gravExt.UserID,
				EmailHash:   gravExt.HashedEmail,
				HashedPhone: gravExt.HashedPhone,
			},
			ExcludedTopics: gravExt.ExcludedTopics,
			Relevancy:      gravExt.Relevancy,
			TestAd:         testAd,
		}

		body, err := json.Marshal(gravReq)
		if err != nil {
			errs = append(errs, fmt.Errorf("gravity: failed to marshal request for imp %s: %w", imp.ID, err))
			continue
		}

		endpointStr, err := macros.ResolveMacros(a.endpoint, macros.EndpointTemplateParams{})
		if err != nil {
			errs = append(errs, fmt.Errorf("gravity: failed to resolve endpoint: %w", err))
			continue
		}

		headers := http.Header{}
		headers.Set("Content-Type", "application/json;charset=utf-8")
		headers.Set("Accept", "application/json")
		headers.Set("Authorization", "Bearer "+a.info.APIKey)

		if request.Device != nil {
			if request.Device.IP != "" {
				headers.Set("X-Forwarded-For", request.Device.IP)
			} else if request.Device.IPv6 != "" {
				headers.Set("X-Forwarded-For", request.Device.IPv6)
			}
			if request.Device.UA != "" {
				headers.Set("User-Agent", request.Device.UA)
			}
		}

		requests = append(requests, &adapters.RequestData{
			Method:  "POST",
			Uri:     endpointStr,
			Body:    body,
			Headers: headers,
			ImpIDs:  openrtb_ext.GetImpIDs(request.Imp),
		})
	}

	return requests, errs
}

// MakeBids translates the Gravity API response back into an OpenRTB BidderResponse.
func (a *adapter) MakeBids(request *openrtb2.BidRequest, _ *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode == http.StatusBadRequest {
		return nil, []error{&errortypes.BadInput{
			Message: fmt.Sprintf("gravity: unexpected status code %d: %s", response.StatusCode, response.Body),
		}}
	}
	if response.StatusCode != http.StatusOK {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("gravity: unexpected status code %d: %s", response.StatusCode, response.Body),
		}}
	}

	var ads []gravityAd
	if err := json.Unmarshal(response.Body, &ads); err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("gravity: failed to parse response: %s", err),
		}}
	}

	if len(ads) == 0 {
		return nil, nil
	}

	if len(request.Imp) == 0 {
		return nil, []error{&errortypes.BadServerResponse{
			Message: "gravity: got bid but request had no imps",
		}}
	}

	ad := &ads[0]
	imp := request.Imp[0]

	bidPrice := a.info.BidPrice
	var bidderExt adapters.ExtImpBidder
	var gravExt openrtb_ext.ExtImpGravity
	if err := json.Unmarshal(imp.Ext, &bidderExt); err == nil {
		if err := json.Unmarshal(bidderExt.Bidder, &gravExt); err == nil {
			if gravExt.BidPrice > 0 {
				bidPrice = gravExt.BidPrice
			}
		}
	}

	nativeAdm, err := buildNativeAdm(ad)
	if err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("gravity: failed to build native adm: %s", err),
		}}
	}

	var adomain []string
	if ad.URL != "" {
		if host := extractHost(ad.URL); host != "" {
			adomain = []string{host}
		}
	}

	ortbBid := openrtb2.Bid{
		ID:      imp.ID,
		ImpID:   imp.ID,
		Price:   bidPrice,
		AdM:     nativeAdm,
		BURL:    ad.ImpURL,
		ADomain: adomain,
		CrID:    ad.PlacementID,
	}

	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(1)
	bidderResponse.Bids = append(bidderResponse.Bids, &adapters.TypedBid{
		Bid:     &ortbBid,
		BidType: openrtb_ext.BidTypeNative,
	})
	bidderResponse.Currency = "USD"

	return bidderResponse, nil
}

// buildNativeAdm constructs an OpenRTB native adm string from a Gravity ad.
//
// Asset IDs are 0-indexed to match what Prebid.js generates from the legacy
// native ad unit definition (title→0, img→1, body→2, sponsoredBy→3).
//
//	0 = title        (required) — ad headline
//	1 = img/logo     (type 2, optional) — brand favicon
//	2 = data/body    (type 2, optional) — ad body copy
//	3 = data/sponsor (type 1, optional) — brand name
//	4 = data/cta     (extra) — call-to-action text
func buildNativeAdm(ad *gravityAd) (string, error) {
	type nativeTitle struct {
		Text string `json:"text"`
	}
	type nativeImg struct {
		URL  string `json:"url"`
		Type int    `json:"type"`
	}
	type nativeData struct {
		Value string `json:"value"`
	}
	type nativeLink struct {
		URL string `json:"url"`
	}
	type nativeAsset struct {
		ID    int          `json:"id"`
		Title *nativeTitle `json:"title,omitempty"`
		Img   *nativeImg   `json:"img,omitempty"`
		Data  *nativeData  `json:"data,omitempty"`
	}
	type nativeAdmWrapper struct {
		Ver         string        `json:"ver"`
		Link        nativeLink    `json:"link"`
		Assets      []nativeAsset `json:"assets"`
		ImpTrackers []string      `json:"imptrackers,omitempty"`
	}

	headline := ad.Title
	if headline == "" {
		headline = ad.AdText
	}

	assets := []nativeAsset{
		{ID: 0, Title: &nativeTitle{Text: headline}},
	}

	if ad.Favicon != "" {
		assets = append(assets, nativeAsset{ID: 1, Img: &nativeImg{URL: ad.Favicon, Type: 2}})
	}

	if ad.AdText != "" {
		assets = append(assets, nativeAsset{ID: 2, Data: &nativeData{Value: ad.AdText}})
	}

	if ad.BrandName != "" {
		assets = append(assets, nativeAsset{ID: 3, Data: &nativeData{Value: ad.BrandName}})
	}

	ctaText := ad.CTA
	if ctaText == "" {
		ctaText = "Learn More"
	}
	assets = append(assets, nativeAsset{ID: 4, Data: &nativeData{Value: ctaText}})

	adm := nativeAdmWrapper{
		Ver:    "1.1",
		Link:   nativeLink{URL: ad.ClickURL},
		Assets: assets,
	}
	if ad.ImpURL != "" {
		adm.ImpTrackers = []string{ad.ImpURL}
	}

	b, err := json.Marshal(adm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// extractHost returns the hostname from a URL, used for adomain.
func extractHost(rawURL string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if len(rawURL) > len(prefix) && rawURL[:len(prefix)] == prefix {
			rest := rawURL[len(prefix):]
			for i, c := range rest {
				if c == '/' || c == '?' || c == '#' {
					return rest[:i]
				}
			}
			return rest
		}
	}
	return ""
}
