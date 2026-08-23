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
// ── Required fields — NOT the same thing as PBS schema "required" ─────────
//
// Request/Response/Timestamp/DeviceContext are the four genuinely dynamic,
// per-auction fields Imprezia's own HTTP API requires — but
// static/bidder-params/imprezia.json deliberately does NOT mark any of
// them as JSON-schema "required" (nothing in this schema is). A real
// production incident (2026-08-22, learnrithm) is why: Request/Response
// are injected per-auction from window.tpc.data.messages once an
// assistant reply exists — exactly analogous to Gravity's `messages`.
// Any auction fired before that (page load, or the moment a prompt is
// submitted but before the reply lands) has no Request/Response to send,
// same as Gravity legitimately having no `messages` yet. PBS's own static
// schema validation runs BEFORE this adapter's MakeRequests ever gets a
// chance to skip gracefully — so declaring these "required" at the schema
// level meant PBS rejected the WHOLE imp (not just Imprezia's bid,
// killing Thrad too on that request) every time. An earlier version of
// this file got this exactly backwards: it required Request/Response
// (the sometimes-absent dynamic fields) while correctly leaving
// userId/sessionId/siteId/placementId optional (the always-present
// static/fallback ones) — the opposite of the safe pattern. The schema
// now requires nothing; MakeRequests below still checks for a non-empty
// Request/Response/Timestamp and a non-nil DeviceContext, and skips the
// imp with a soft per-bidder error (no bid, no hard failure) when any is
// missing — same graceful-skip contract Gravity already has for
// `messages`. Do NOT add any of these to
// static/bidder-params/imprezia.json's schema.
//
// SourceURL and PlatformString are also required by Imprezia's API but
// are NOT dynamic client fields — MakeRequests derives them itself from
// request.Site.Page and a hardcoded "browser" respectively (only
// web-bundle chat placements are live; daimon/mobile isn't), so they
// need no schema entry, client bundle change, or graceful-skip check.
//
// X-Forwarded-User-Agent and X-Forwarded-For are two more fields
// Imprezia's docs require, sent as headers rather than body fields — set
// by MakeRequests from request.Device.UA/IP, which PBS itself already
// populates with real end-user values (confirmed live 2026-08-23), never
// this adapter's own HTTP client identity.
//
// All six of the above were added 2026-08-23 after Imprezia inspected
// real production traffic (once their test campaign went live and their
// account's earlier 403 cleared) and reported them missing. Every real
// auction was getting a clean `"ad": null` no-fill before this fix — not
// an error, decisioning just running on an incomplete signal set per
// their Chat Ads API REST reference (the "Chat Ads API" platform tab at
// https://demo.imprezia.ai/docs — distinct from the Web SDK tab that
// renders by default and is what this adapter was originally built
// against).
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
// Production's earlier 403 (partner_chat_ads_not_enabled) cleared
// 2026-08-23 — confirmed live (200s, no auth errors) — but see the
// no-fill note above; sandbox and production are documented to share the
// same API surface.
//
// ── Impression tracking: two required beacons + tracker timing ────────────
//
// Reworked 2026-08-23. Imprezia reported seeing zero impression beacons
// for ads we'd served: it turned out we'd never implemented their
// required billing mechanism at all. Two distinct things now happen,
// both driven by the ext.imprezia block buildNativeAdm attaches to the
// native adm (see below) — the client bundle (prebid-deployments) reads
// it, gated on bid.meta.adapterCode === 'imprezia':
//
//  1. Tracker delivery — ad.trackers.impression[]/mrc50[] (or their
//     mutually-exclusive frame-URL equivalents) used to be flattened into
//     one `imptrackers` array that only ever fired once, at MRC50-viewable
//     — so trackers.impression never fired at the right time (insertion),
//     and neither fired at all on an ad that never reached 50%
//     visibility. Now emitted as OpenRTB Native eventtrackers (event:1 =
//     impression, event:2 = viewable-mrc50) so the client can fire each
//     at its correct moment.
//  2. Impression beacons — Imprezia's actual billing signal. The client
//     must POST to {beaconBaseUrl}/v1/events/sdk-impression twice per
//     rendered ad: once at insertion (eventType sdk_impression_inserted,
//     telemetry only) and once at MRC50-viewable (eventType
//     sdk_impression — the billable one), echoing requestId,
//     impressionUuid, servedAt, publisherId, and beaconToken (when
//     present) back exactly as received. ext.imprezia carries all of
//     these plus beaconBaseUrl (sandbox vs prod, since the client has no
//     other way to know which host was used) so the client never has to
//     guess or re-derive anything.

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
//
// Timestamp, SourceURL, PlatformString, and DeviceContext were added
// 2026-08-23 after Imprezia inspected our real production traffic and
// reported them missing — their Chat Ads API REST reference (the
// "Chat Ads API" platform tab at https://demo.imprezia.ai/docs, distinct
// from the Web SDK tab that renders by default) marks all four required
// on every request, alongside two headers (see MakeRequests) we also
// weren't sending. Every real auction against learnrithm's Stored Imp
// was getting a clean `"ad": null` no-fill from Imprezia's prod API
// before this fix — not an error, just decisioning running on an
// incomplete signal set.
type impreziaRequest struct {
	Request        string         `json:"request"`
	Response       string         `json:"response"`
	Timestamp      string         `json:"timestamp"`
	SourceURL      string         `json:"sourceUrl"`
	PlatformString string         `json:"platformString"`
	DeviceContext  *deviceContext `json:"deviceContext,omitempty"`
	UserID         string         `json:"userId,omitempty"`
	SessionID      string         `json:"sessionId,omitempty"`
	SiteID         string         `json:"siteId,omitempty"`
	PlacementID    string         `json:"placementId,omitempty"`
	MaxCards       int            `json:"maxCards,omitempty"`
}

// deviceContext is Imprezia's required `{deviceType, viewportWidth,
// viewportHeight}` object. Sourced from ExtImpImprezia.DeviceContext,
// which the client bundle populates per-auction from window.innerWidth/
// innerHeight (viewport, not a specific ad container — see
// prebid-deployments' buildImpreziaParams() for why) and a simple width
// breakpoint for DeviceType.
type deviceContext struct {
	DeviceType     string `json:"deviceType"`
	ViewportWidth  int    `json:"viewportWidth"`
	ViewportHeight int    `json:"viewportHeight"`
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

// Exactly one channel is present per serve: either the array fields
// (Impression/MRC50, fired as plain pixel GETs) or the frame fields
// (ImpressionFrameURL/ViewabilityFrameURL, embedded as a hidden iframe
// instead) — never both for the same moment. See buildNativeAdm's ext
// block, which carries whichever channel is present through to the
// client unchanged.
type trackers struct {
	Impression          []string `json:"impression,omitempty"`
	MRC50               []string `json:"mrc50,omitempty"`
	ImpressionFrameURL  string   `json:"impressionFrameUrl,omitempty"`
	ViewabilityFrameURL string   `json:"viewabilityFrameUrl,omitempty"`
}

type impression struct {
	ImpressionUUID string       `json:"impressionUuid,omitempty"`
	BeaconToken    *beaconToken `json:"beaconToken,omitempty"`
	ServedAt       string       `json:"servedAt,omitempty"`
	PublisherID    string       `json:"publisherId,omitempty"`
}

// beaconToken is not on every response (echoed only when Imprezia
// returns one) — added 2026-08-23 alongside the impression-beacon
// rework. Previously unparsed entirely.
type beaconToken struct {
	Token    string `json:"token"`
	IssuedAt int64  `json:"issuedAt"`
	Kid      string `json:"kid"`
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
		// Timestamp and DeviceContext are the other two genuinely dynamic
		// (client-per-turn) fields Imprezia's real API requires, added
		// 2026-08-23 — same graceful-skip contract as Request/Response
		// above, and same reason neither is schema-"required": an auction
		// fired before the client bundle has them yet must not take down
		// the whole imp (Thrad included). See bidder-params/imprezia.json.
		if impExt.Timestamp == "" {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("imprezia: timestamp is required for imp %s", imp.ID),
			})
			continue
		}
		if impExt.DeviceContext == nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("imprezia: deviceContext is required for imp %s", imp.ID),
			})
			continue
		}

		maxCards := defaultMaxCards
		if impExt.MaxCards != nil && *impExt.MaxCards > 0 {
			maxCards = *impExt.MaxCards
		}

		impReq := impreziaRequest{
			Request:        impExt.Request,
			Response:       impExt.Response,
			Timestamp:      impExt.Timestamp,
			SourceURL:      sourceURLFromRequest(request),
			PlatformString: "browser", // only web-bundle chat placements are live; daimon/mobile isn't (see FORK_NOTES / docs)
			DeviceContext: &deviceContext{
				DeviceType:     impExt.DeviceContext.DeviceType,
				ViewportWidth:  impExt.DeviceContext.ViewportWidth,
				ViewportHeight: impExt.DeviceContext.ViewportHeight,
			},
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
		// Real end-user UA/IP — request.Device.UA/IP are already populated
		// with real values by PBS itself (confirmed live 2026-08-23, not
		// this adapter's or the client bundle's doing), just never
		// forwarded to Imprezia before now. Per Imprezia's own docs: omit
		// rather than send a placeholder when unavailable — never send our
		// own request's identity.
		if request.Device != nil {
			if request.Device.UA != "" {
				headers.Set("X-Forwarded-User-Agent", request.Device.UA)
			}
			if ip := request.Device.IP; ip != "" {
				headers.Set("X-Forwarded-For", ip)
			} else if request.Device.IPv6 != "" {
				headers.Set("X-Forwarded-For", request.Device.IPv6)
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

	// Same isTest switch MakeRequests used to pick the endpoint/key — the
	// client needs the matching base URL to know where to POST the two
	// impression beacons (see docs' "Base URLs" table), and has no other
	// way to tell sandbox from prod itself.
	var beaconBaseURL string
	if request.Test == 1 {
		beaconBaseURL = baseURLFromEndpoint(a.info.SandboxEndpoint)
	} else {
		resolved, err := macros.ResolveMacros(a.endpoint, macros.EndpointTemplateParams{})
		if err == nil {
			beaconBaseURL = baseURLFromEndpoint(resolved)
		}
	}

	nativeAdm, err := buildNativeAdm(chatResp.Ad, chatResp.RequestID, beaconBaseURL, impExt.SessionID)
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
// Reworked 2026-08-23: ad.trackers.impression[] and ad.trackers.mrc50[]
// used to be flattened into one `imptrackers` array that only ever fired
// once, at MRC50-viewable — so trackers.impression never fired at the
// right time (insertion), and neither fired at all on an ad that never
// reached 50% visibility. Now emitted as spec-correct OpenRTB Native
// `eventtrackers`: event:1 (impression) per trackers.impression URL,
// event:2 (viewable-mrc50) per trackers.mrc50 URL — the render pipeline
// (prebid-deployments' config.js) fires event:1 at insertion and event:2
// at MRC50-viewable, reading the raw parsed object at
// bid.native.ortb.eventtrackers, the same bid.native.ortb.* side-channel
// already relied on for link.clicktrackers. The frame-delivered channel
// (mutually exclusive with the array channel per serve) and the
// beacon-echo metadata the client needs for the two new required
// impression-beacon POSTs (see docs) go in a top-level `ext.imprezia`
// block, valid per OpenRTB Native 1.2's native-response `ext`.
func buildNativeAdm(a *ad, requestID, beaconBaseURL, sessionID string) (string, error) {
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
	type nativeEventTracker struct {
		Event  int    `json:"event"`
		Method int    `json:"method"`
		URL    string `json:"url"`
	}
	type impreziaBeaconToken struct {
		Token    string `json:"token"`
		IssuedAt int64  `json:"issuedAt"`
		Kid      string `json:"kid"`
	}
	type impreziaExt struct {
		BeaconBaseURL       string               `json:"beaconBaseUrl"`
		RequestID           string               `json:"requestId"`
		SessionID           string               `json:"sessionId,omitempty"`
		ImpressionUUID      string               `json:"impressionUuid,omitempty"`
		ServedAt            string               `json:"servedAt,omitempty"`
		PublisherID         string               `json:"publisherId,omitempty"`
		BeaconToken         *impreziaBeaconToken `json:"beaconToken,omitempty"`
		ImpressionFrameURL  string               `json:"impressionFrameUrl,omitempty"`
		ViewabilityFrameURL string               `json:"viewabilityFrameUrl,omitempty"`
	}
	type nativeExt struct {
		Imprezia impreziaExt `json:"imprezia"`
	}
	type nativeAdmWrapper struct {
		Ver           string               `json:"ver"`
		Link          nativeLink           `json:"link"`
		Assets        []nativeAsset        `json:"assets"`
		EventTrackers []nativeEventTracker `json:"eventtrackers,omitempty"`
		Ext           nativeExt            `json:"ext"`
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

	var eventTrackers []nativeEventTracker
	for _, url := range a.Trackers.Impression {
		eventTrackers = append(eventTrackers, nativeEventTracker{Event: 1, Method: 1, URL: url})
	}
	for _, url := range a.Trackers.MRC50 {
		eventTrackers = append(eventTrackers, nativeEventTracker{Event: 2, Method: 1, URL: url})
	}

	var tok *impreziaBeaconToken
	if a.Impression.BeaconToken != nil {
		tok = &impreziaBeaconToken{
			Token:    a.Impression.BeaconToken.Token,
			IssuedAt: a.Impression.BeaconToken.IssuedAt,
			Kid:      a.Impression.BeaconToken.Kid,
		}
	}

	adm := nativeAdmWrapper{
		Ver:           "1.1",
		Link:          nativeLink{URL: a.ClickURL},
		Assets:        assets,
		EventTrackers: eventTrackers,
		Ext: nativeExt{Imprezia: impreziaExt{
			BeaconBaseURL:       beaconBaseURL,
			RequestID:           requestID,
			SessionID:           sessionID,
			ImpressionUUID:      a.Impression.ImpressionUUID,
			ServedAt:            a.Impression.ServedAt,
			PublisherID:         a.Impression.PublisherID,
			BeaconToken:         tok,
			ImpressionFrameURL:  a.Trackers.ImpressionFrameURL,
			ViewabilityFrameURL: a.Trackers.ViewabilityFrameURL,
		}},
	}

	b, err := json.Marshal(adm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// sourceURLFromRequest returns request.Site.Page with any query string or
// fragment stripped — Imprezia's sourceUrl wants origin + path only. Only
// Site is handled (not App): only web-bundle chat placements are live
// today, same reason PlatformString is hardcoded to "browser" above.
func sourceURLFromRequest(request *openrtb2.BidRequest) string {
	if request.Site == nil {
		return ""
	}
	page := request.Site.Page
	for i, c := range page {
		if c == '?' || c == '#' {
			return page[:i]
		}
	}
	return page
}

// baseURLFromEndpoint strips the known "/v1/ads/chat" suffix from a
// configured endpoint (prod or sandbox) to get the base URL the client
// needs for POSTing impression beacons to {baseUrl}/v1/events/sdk-impression.
func baseURLFromEndpoint(endpoint string) string {
	const suffix = "/v1/ads/chat"
	if len(endpoint) > len(suffix) && endpoint[len(endpoint)-len(suffix):] == suffix {
		return endpoint[:len(endpoint)-len(suffix)]
	}
	return endpoint
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
