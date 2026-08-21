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
// ── Response shape: UNVERIFIED as of this writing ──────────────────────────
//
// Imprezia's docs state their SDK's MonetizeResponse type (monetizedResponse/
// linkData/originalResponse/metadata) nests identically "at the top level of
// a Chat Ads response" — but the account has been returning
// 403 partner_chat_ads_not_enabled on every /v1/ads/chat call so far
// (confirmed after a 100-request warmup batch), so this has never been
// checked against a real response body. monetizeResponse below is written
// to match their documented types exactly; if a real response doesn't
// match, rewrite this struct, don't patch around a wrong assumption — see
// the live-verification checklist in the Imprezia section of
// docs/integration/internal-onboarding.md before wiring this adapter into
// any real Stored Imp.
//
// ── Impression tracking: open question, not resolved here ─────────────────
//
// Gravity/Thrad both return a flat impression-pixel URL we fire ourselves
// (Gravity's ad.ImpURL). Imprezia's confirmed types have no such field —
// LinkData.hyperlink is a tracking-wrapped CLICK url only. Impression
// tracking appears to route through a browser-SDK-specific
// POST /v1/events/sdk-impression endpoint keyed by impressionUuid/
// trackingId, with no confirmed server-to-server equivalent. Do not guess
// a pixel location — MakeBids ships v1 with no imptrackers. Confirm once a
// real response is captured (see live-verification checklist).

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

// defaultMaxCards is what we request when ExtImpImprezia.MaxCards is unset —
// one PBS bid per imp, matching how Gravity/Thrad each produce a single bid.
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

// monetizeResponse mirrors Imprezia's documented MonetizeResponse SDK type —
// see the UNVERIFIED note in the package doc above.
type monetizeResponse struct {
	MonetizedResponse string              `json:"monetizedResponse"`
	LinkData          map[string]linkData `json:"linkData"`
	OriginalResponse  string              `json:"originalResponse"`
	Metadata          monetizeMetadata    `json:"metadata"`
}

type monetizeMetadata struct {
	UserID         string  `json:"userId,omitempty"`
	SessionID      string  `json:"sessionId,omitempty"`
	Timestamp      string  `json:"timestamp,omitempty"`
	RequestID      string  `json:"requestId,omitempty"`
	ProcessingTime float64 `json:"processingTime,omitempty"`
	PublisherID    string  `json:"publisherId,omitempty"`
}

type linkData struct {
	StringLinkWord string            `json:"string_link_word"`
	Hyperlink      string            `json:"hyperlink"`
	TrackingID     string            `json:"trackingId"`
	LinkType       string            `json:"linkType,omitempty"`
	OriginalURL    string            `json:"originalUrl,omitempty"`
	CTAText        string            `json:"ctaText,omitempty"`
	Metadata       *linkDataMetadata `json:"metadata,omitempty"`
}

type linkDataMetadata struct {
	AffiliateID    string        `json:"affiliateId,omitempty"`
	Commission     float64       `json:"commission,omitempty"`
	BrandCategory  string        `json:"brandCategory,omitempty"`
	PlacementType  string        `json:"placementType,omitempty"`
	CardMetadata   *cardMetadata `json:"cardMetadata,omitempty"`
	ImpressionUUID string        `json:"impressionUuid,omitempty"`
}

type cardMetadata struct {
	Title           string  `json:"title"`
	Description     string  `json:"description,omitempty"`
	BrandName       string  `json:"brandName"`
	LogoURL         string  `json:"logoUrl,omitempty"`
	CTAText         string  `json:"ctaText,omitempty"`
	BackgroundColor string  `json:"backgroundColor,omitempty"`
	AdAssetURL      *string `json:"adAssetUrl,omitempty"`
	AdBannerHTML    string  `json:"adBannerHtml,omitempty"`
	AdBannerWidth   int     `json:"adBannerWidth,omitempty"`
	AdBannerHeight  int     `json:"adBannerHeight,omitempty"`
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

	var mResp monetizeResponse
	if err := json.Unmarshal(response.Body, &mResp); err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("imprezia: failed to parse response: %s", err),
		}}
	}

	if len(mResp.LinkData) == 0 {
		return nil, nil
	}

	if len(request.Imp) == 0 {
		return nil, []error{&errortypes.BadServerResponse{
			Message: "imprezia: got bid but request had no imps",
		}}
	}

	link := selectLinkData(mResp.LinkData)
	if link == nil {
		return nil, nil
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

	nativeAdm, err := buildNativeAdm(link)
	if err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("imprezia: failed to build native adm: %s", err),
		}}
	}

	var adomain []string
	domainSource := ""
	if link.OriginalURL != "" {
		domainSource = link.OriginalURL
	} else if link.Hyperlink != "" {
		domainSource = link.Hyperlink
	}
	if host := extractHost(domainSource); host != "" {
		adomain = []string{host}
	}

	ortbBid := openrtb2.Bid{
		ID:      imp.ID,
		ImpID:   imp.ID,
		Price:   bidPrice,
		AdM:     nativeAdm,
		ADomain: adomain,
		CrID:    link.TrackingID,
	}

	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(1)
	bidderResponse.Bids = append(bidderResponse.Bids, &adapters.TypedBid{
		Bid:     &ortbBid,
		BidType: openrtb_ext.BidTypeNative,
	})
	bidderResponse.Currency = "USD"

	return bidderResponse, nil
}

// selectLinkData picks the sponsored-card entry from the linkData map. Map
// iteration order isn't guaranteed in Go/JSON, so prefer an entry carrying
// CardMetadata (the actual ad card, as opposed to a plain inline text link)
// when more than one entry is present; otherwise take whichever is first.
func selectLinkData(m map[string]linkData) *linkData {
	var fallback *linkData
	for k := range m {
		entry := m[k]
		if fallback == nil {
			fallback = &entry
		}
		if entry.Metadata != nil && entry.Metadata.CardMetadata != nil {
			return &entry
		}
	}
	return fallback
}

// buildNativeAdm constructs an OpenRTB native adm string from an Imprezia
// LinkData/CardMetadata pair.
//
// Same 5-asset 0-4 scheme as Gravity/Thrad, to match what Prebid.js expects
// from the legacy native ad unit definition (title→0, img→1, body→2,
// sponsoredBy→3, cta→4):
//
//	0 = title        (required) — card headline
//	1 = img/logo     (type 3, optional) — brand logo
//	2 = data/body    (type 2, optional) — card description
//	3 = data/sponsor (type 1, optional) — brand name
//	4 = data/cta     (extra) — call-to-action text
//
// No imptrackers — see the package doc's "Impression tracking" note.
func buildNativeAdm(link *linkData) (string, error) {
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
		Ver    string        `json:"ver"`
		Link   nativeLink    `json:"link"`
		Assets []nativeAsset `json:"assets"`
	}

	var card *cardMetadata
	if link.Metadata != nil {
		card = link.Metadata.CardMetadata
	}

	headline := link.StringLinkWord
	if card != nil && card.Title != "" {
		headline = card.Title
	}

	assets := []nativeAsset{
		{ID: 0, Title: &nativeTitle{Text: headline}},
	}

	if card != nil {
		if card.LogoURL != "" {
			assets = append(assets, nativeAsset{ID: 1, Img: &nativeImg{URL: card.LogoURL, Type: 3}})
		}
		if card.Description != "" {
			assets = append(assets, nativeAsset{ID: 2, Data: &nativeData{Value: card.Description}})
		}
		if card.BrandName != "" {
			assets = append(assets, nativeAsset{ID: 3, Data: &nativeData{Value: card.BrandName}})
		}
	}

	ctaText := link.CTAText
	if card != nil && card.CTAText != "" {
		ctaText = card.CTAText
	}
	if ctaText == "" {
		ctaText = "Learn More"
	}
	assets = append(assets, nativeAsset{ID: 4, Data: &nativeData{Value: ctaText}})

	adm := nativeAdmWrapper{
		Ver:    "1.1",
		Link:   nativeLink{URL: link.Hyperlink},
		Assets: assets,
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
