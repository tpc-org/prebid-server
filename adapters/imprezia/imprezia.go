package imprezia

// Package imprezia implements a Prebid Server adapter for Imprezia
// (imprezia.ai) — chat-context native ads via their "Chat Ads API".
//
// ── API keys and hosts ──────────────────────────────────────────────────────
//
// Unlike Gravity (one key, one host) and Thrad (one key pair, one host,
// switched by `test`), Imprezia has one global account key PER environment
// AND a different HOST per environment:
//
//	prod:    https://api.imprezia.ai/v1/ads/chat         api_pub_prod_...
//	sandbox: https://api-sandbox.imprezia.ai/v1/ads/chat  api_pub_sandbox_...
//
// Configured in pbs-settings/pbs.yaml under adapters.imprezia:
//
//	adapters:
//	  imprezia:
//	    endpoint: "https://api.imprezia.ai/v1/ads/chat"
//	    extra_info: '{"apiKey":"...","sandboxApiKey":"...","sandboxEndpoint":"https://api-sandbox.imprezia.ai/v1/ads/chat","bidPrice":1.0}'
//
// cfg.Endpoint (the production endpoint) is resolved via the normal
// macros.ResolveMacros path exactly like Gravity/Thrad. The sandbox
// endpoint/key are used verbatim (no macro templating) when
// BidRequest.Test == 1.
//
// Sandbox and production are separate account namespaces — a real
// production siteId (e.g. sayhola's) is rejected by the sandbox endpoint
// with 400 "Invalid publisher hierarchy" (confirmed live). Don't test a
// real siteId against the sandbox host expecting it to work.
//
// ── No price field ───────────────────────────────────────────────────────
//
// Same gap as Gravity: Imprezia's response has no price/CPM anywhere.
// extra_info.bidPrice is the CPM (USD) reported to PBS, default 1.0 if
// omitted — overridable per-imp via ExtImpImprezia.BidPrice. See
// docs/integration/internal-onboarding.md's "Bid price calibration"
// section for the process to keep this tracking real observed eCPM once
// traffic exists (same process now also applies to Gravity).
//
// ── Required fields ───────────────────────────────────────────────────────
//
// Only Request/Response are required by Imprezia's own API — everything
// else (userId, sessionId, siteId, placementId) is genuinely optional.
// This is the OPPOSITE of Gravity, where userId/sessionId/placement/
// placementId are all required — do not copy Gravity's required-field
// list here. See static/bidder-params/imprezia.json's own comment: this
// is a deliberate fix for the incident class documented in
// docs/runbooks/gravity-reactivation.md (PBS rejects the WHOLE imp, not
// just one bidder, when a multi-bidder stored imp is missing a
// bidder-params-declared-required field).
//
// ── Response shape: confirmed live against sandbox 2026-08-21 ─────────────
//
// Imprezia's own docs claim their SDK's MonetizeResponse type
// (monetizedResponse/linkData/originalResponse/metadata) maps onto the raw
// Chat Ads API response — this turned out to be WRONG. The real
// POST /v1/ads/chat response (confirmed via multiple live sandbox calls,
// varying query content and maxCards) is a flat, RTB-shaped single-ad
// object, not the SDK's multi-card-embedded-in-text shape:
//
//	{
//	  "requestId": "req_...", "siteId": "uuid", "placementId": null|"string",
//	  "ad": {
//	    "creative": {"brandName","title","description","cta","imageUrl"},
//	    "clickUrl": "https://go-sandbox.imprezia.ai/go/...",
//	    "trackers": {"impression": ["https://..."], "mrc50": ["https://..."]},
//	    "impression": {"impressionUuid","beaconToken":{...},"servedAt","publisherId"}
//	  }
//	}
//
// maxCards has no effect on this shape — confirmed maxCards:2 still
// returns a single "ad" object, never an array. Sandbox always returned
// the same fixed Imprezia house-ad creative regardless of query content
// (normal sandbox behavior — a real no-fill case, i.e. a response with no
// "ad" key at all, has not been observed but is handled defensively
// below). Two distinct error shapes exist: 403 auth/authz errors nest
// under `{"error":{"type","code","message"}}` (see MakeBids' status
// handling below, which just embeds the raw body rather than parsing
// either shape); 400 validation errors are flatter,
// `{"error":"string","message":"string"}`.
//
// Not yet independently re-confirmed against the *production* host/key
// (still blocked on 403 partner_chat_ads_not_enabled as of this writing)
// — sandbox and production are documented to share the same API surface,
// but re-verify prod once its 403 clears rather than assuming.
//
// ── Impression tracking: resolved ──────────────────────────────────────────
//
// ad.trackers.impression[] are real, plain, fireable pixel URLs (confirmed
// live) — fired via imptrackers in the native adm below, same as
// Gravity/Thrad. ad.trackers.mrc50[] (MRC 50%-viewability trackers) are
// fired the same way — this codebase's native adm has no separate
// viewability-tracker slot, and every entry in imptrackers gets fired
// together by the rendering side regardless of which sub-category it
// came from. ad.impression.beaconToken is a distinct, more complex signed
// server-to-server postback mechanism that duplicates what the plain
// tracker URLs already give us — not used here.

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
	APIKey          string  `json:"apiKey"`
	SandboxAPIKey   string  `json:"sandboxApiKey"`
	SandboxEndpoint string  `json:"sandboxEndpoint"`
	BidPrice        float64 `json:"bidPrice"`
}

// defaultMaxCards is what we request when ExtImpImprezia.MaxCards is unset.
// Confirmed live that Imprezia's Chat Ads API always returns a single "ad"
// object regardless of this value — kept only because it's a documented
// request field, not because it changes response shape on our side.
const defaultMaxCards = 1

// ── Imprezia API types ─────────────────────────────────────────────────────────

// impreziaRequest is the flat POST /v1/ads/chat request body.
type impreziaRequest struct {
	Request     string `json:"request"`
	Response    string `json:"response"`
	UserID      string `json:"userId,omitempty"`
	SessionID   string `json:"sessionId,omitempty"`
	SiteID      string `json:"siteId,omitempty"`
	PlacementID string `json:"placementId,omitempty"`
	MaxCards    int    `json:"maxCards,omitempty"`
}

// chatAdsResponse is the real, confirmed POST /v1/ads/chat response shape
// — see the package doc's "Response shape" section.
type chatAdsResponse struct {
	RequestID   string  `json:"requestId"`
	SiteID      string  `json:"siteId"`
	PlacementID *string `json:"placementId"`
	Ad          *ad     `json:"ad"`
}

type ad struct {
	Creative   creative   `json:"creative"`
	ClickURL   string     `json:"clickUrl"`
	Trackers   trackers   `json:"trackers"`
	Impression impression `json:"impression"`
}

type creative struct {
	BrandName   string `json:"brandName"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	CTA         string `json:"cta,omitempty"`
	ImageURL    string `json:"imageUrl,omitempty"`
}

type trackers struct {
	Impression []string `json:"impression,omitempty"`
	MRC50      []string `json:"mrc50,omitempty"`
}

type impression struct {
	ImpressionUUID string `json:"impressionUuid,omitempty"`
	ServedAt       string `json:"servedAt,omitempty"`
	PublisherID    string `json:"publisherId,omitempty"`
}

// ── Adapter ───────────────────────────────────────────────────────────────────

type adapter struct {
	endpoint *template.Template
	info     extraInfo
}

func Builder(bidderName openrtb_ext.BidderName, cfg config.Adapter, server config.Server) (adapters.Bidder, error) {
	tmpl, err := template.New("endpointTemplate").Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("imprezia: unable to parse endpoint template: %w", err)
	}

	a := &adapter{endpoint: tmpl}

	if cfg.ExtraAdapterInfo != "" {
		if err := json.Unmarshal([]byte(cfg.ExtraAdapterInfo), &a.info); err != nil {
			return nil, fmt.Errorf("imprezia: unable to parse extra_info: %w", err)
		}
	}
	if a.info.APIKey == "" {
		return nil, fmt.Errorf("imprezia: extra_info must contain apiKey")
	}
	if a.info.BidPrice <= 0 {
		a.info.BidPrice = 1.0
	}

	return a, nil
}

// MakeRequests translates the OpenRTB auction into one Imprezia API call per imp.
func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var requests []*adapters.RequestData
	var errs []error

	isTest := request.Test == 1

	for i := range request.Imp {
		imp := request.Imp[i]

		var bidderExt adapters.ExtImpBidder
		if err := json.Unmarshal(imp.Ext, &bidderExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("imprezia: invalid imp.ext for imp %s: %s", imp.ID, err),
			})
			continue
		}

		var impExt openrtb_ext.ExtImpImprezia
		if err := json.Unmarshal(bidderExt.Bidder, &impExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("imprezia: invalid imp.ext.bidder for imp %s: %s", imp.ID, err),
			})
			continue
		}

		if impExt.Request == "" {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("imprezia: request is required for imp %s", imp.ID),
			})
			continue
		}
		if impExt.Response == "" {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("imprezia: response is required for imp %s", imp.ID),
			})
			continue
		}

		maxCards := defaultMaxCards
		if impExt.MaxCards != nil && *impExt.MaxCards > 0 {
			maxCards = *impExt.MaxCards
		}

		impReq := impreziaRequest{
			Request:     impExt.Request,
			Response:    impExt.Response,
			UserID:      impExt.UserID,
			SessionID:   impExt.SessionID,
			SiteID:      impExt.SiteID,
			PlacementID: impExt.PlacementID,
			MaxCards:    maxCards,
		}

		body, err := json.Marshal(impReq)
		if err != nil {
			errs = append(errs, fmt.Errorf("imprezia: failed to marshal request for imp %s: %w", imp.ID, err))
			continue
		}

		var endpointStr, apiKey string
		if isTest {
			if a.info.SandboxEndpoint == "" || a.info.SandboxAPIKey == "" {
				errs = append(errs, fmt.Errorf("imprezia: test mode requires sandboxEndpoint and sandboxApiKey in extra_info"))
				continue
			}
			endpointStr = a.info.SandboxEndpoint
			apiKey = a.info.SandboxAPIKey
		} else {
			resolved, err := macros.ResolveMacros(a.endpoint, macros.EndpointTemplateParams{})
			if err != nil {
				errs = append(errs, fmt.Errorf("imprezia: failed to resolve endpoint: %w", err))
				continue
			}
			endpointStr = resolved
			apiKey = a.info.APIKey
		}

		headers := http.Header{}
		headers.Set("Content-Type", "application/json")
		headers.Set("Accept", "application/json")
		headers.Set("X-API-Key", apiKey)

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

// MakeBids translates the Imprezia API response back into an OpenRTB BidderResponse.
func (a *adapter) MakeBids(request *openrtb2.BidRequest, requestData *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode == http.StatusBadRequest {
		return nil, []error{&errortypes.BadInput{
			Message: fmt.Sprintf("imprezia: unexpected status code %d: %s", response.StatusCode, response.Body),
		}}
	}
	if response.StatusCode != http.StatusOK {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("imprezia: unexpected status code %d: %s", response.StatusCode, response.Body),
		}}
	}

	var chatResp chatAdsResponse
	if err := json.Unmarshal(response.Body, &chatResp); err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("imprezia: failed to parse response: %s", err),
		}}
	}

	if chatResp.Ad == nil {
		return nil, nil
	}

	if len(request.Imp) == 0 {
		return nil, []error{&errortypes.BadServerResponse{
			Message: "imprezia: got bid but request had no imps",
		}}
	}

	imp := request.Imp[0]

	bidPrice := a.info.BidPrice
	var bidderExt adapters.ExtImpBidder
	var impExt openrtb_ext.ExtImpImprezia
	if err := json.Unmarshal(imp.Ext, &bidderExt); err == nil {
		if err := json.Unmarshal(bidderExt.Bidder, &impExt); err == nil {
			if impExt.BidPrice > 0 {
				bidPrice = impExt.BidPrice
			}
		}
	}

	nativeAdm, err := buildNativeAdm(chatResp.Ad)
	if err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("imprezia: failed to build native adm: %s", err),
		}}
	}

	var adomain []string
	if host := extractHost(chatResp.Ad.ClickURL); host != "" {
		adomain = []string{host}
	}

	ortbBid := openrtb2.Bid{
		ID:      imp.ID,
		ImpID:   imp.ID,
		Price:   bidPrice,
		AdM:     nativeAdm,
		ADomain: adomain,
		CrID:    chatResp.Ad.Impression.ImpressionUUID,
	}

	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(1)
	bidderResponse.Bids = append(bidderResponse.Bids, &adapters.TypedBid{
		Bid:     &ortbBid,
		BidType: openrtb_ext.BidTypeNative,
	})
	bidderResponse.Currency = "USD"

	return bidderResponse, nil
}

// buildNativeAdm constructs an OpenRTB native adm string from an Imprezia
// ad object.
//
// Same 5-asset 0-4 scheme as Gravity/Thrad, to match what Prebid.js expects
// from the legacy native ad unit definition (title→0, img→1, body→2,
// sponsoredBy→3, cta→4):
//
//	0 = title        (required) — creative headline
//	1 = img          (type 3, optional) — creative image
//	2 = data/body    (type 2, optional) — creative description
//	3 = data/sponsor (type 1, optional) — brand name
//	4 = data/cta     (extra) — call-to-action text
//
// imptrackers fires both ad.trackers.impression[] and ad.trackers.mrc50[]
// — confirmed real, plain, fireable pixel URLs (see package doc).
func buildNativeAdm(a *ad) (string, error) {
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

	assets := []nativeAsset{
		{ID: 0, Title: &nativeTitle{Text: a.Creative.Title}},
	}

	if a.Creative.ImageURL != "" {
		assets = append(assets, nativeAsset{ID: 1, Img: &nativeImg{URL: a.Creative.ImageURL, Type: 3}})
	}
	if a.Creative.Description != "" {
		assets = append(assets, nativeAsset{ID: 2, Data: &nativeData{Value: a.Creative.Description}})
	}
	if a.Creative.BrandName != "" {
		assets = append(assets, nativeAsset{ID: 3, Data: &nativeData{Value: a.Creative.BrandName}})
	}

	ctaText := a.Creative.CTA
	if ctaText == "" {
		ctaText = "Learn More"
	}
	assets = append(assets, nativeAsset{ID: 4, Data: &nativeData{Value: ctaText}})

	var impTrackers []string
	impTrackers = append(impTrackers, a.Trackers.Impression...)
	impTrackers = append(impTrackers, a.Trackers.MRC50...)

	adm := nativeAdmWrapper{
		Ver:         "1.1",
		Link:        nativeLink{URL: a.ClickURL},
		Assets:      assets,
		ImpTrackers: impTrackers,
	}

	b, err := json.Marshal(adm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// extractHost returns the hostname from a URL, used for adomain. Copied
// (not shared) from Gravity/Thrad's own unexported helpers, matching the
// existing non-DRY convention between the demand-partner adapters in this
// fork.
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
