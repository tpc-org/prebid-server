package thrad

// Package thrad implements a Prebid Server adapter for Thrad (thrads.ai).
//
// ── Key selection (per-publisher, then staging vs production) ────────────────
//
// Thrad now issues a separate "Publisher" (with its own production + staging
// key pair) per TPC publisher, rather than one account-wide key. Keys are
// configured in pbs-settings/pbs.yaml under adapters.thrad.extra_info as a
// default pair plus a per-publisher override map, keyed by our own
// Publisher.slug (sayhola, drawify, learnrithm, artsmart, slashspace — the
// same identifier used everywhere else for these publishers):
//
//	adapters:
//	  thrad:
//	    endpoint: "https://ssp.thrads.ai/api/v1/ssp/bid-request"
//	    extra_info: '{"productionKey":"pk_live_default","stagingKey":"pk_staging_default","publishers":{"drawify":{"productionKey":"pk_live_drawify","stagingKey":"pk_staging_drawify"}}}'
//
// The top-level productionKey/stagingKey are a fallback for any imp whose
// Stored Imp doesn't (yet) carry a publisherId — this keeps every publisher
// onboarded before this per-publisher scheme existed working unchanged.
// ExtImpThrads.PublisherID (ext.prebid.bidder.thrad.publisherId in the Stored
// Imp) selects which entry in the "publishers" map applies; when absent, or
// when the map has no entry for it, the top-level default is used instead.
//
// Within whichever key pair is selected, the adapter still picks stagingKey
// vs productionKey based on BidRequest.Test = 1 (set automatically by
// Prebid.js when ?pbjs_debug=true) exactly as before.
//
// ── Conversational context ───────────────────────────────────────────────────
//
// Thrad requires userId, chatId, and messages per auction.
// These are dynamic (change every auction) and cannot live in a Stored Imp.
// Publishers pass them via window.tpc.data; each client bundle reads them
// and injects them into params.thrad, which tpcBidAdapter forwards as
// imp.ext.prebid.bidder.thrad in the OpenRTB request.
// PBS merges that with the Stored Imp's static thrad params (including
// publisherId, see above) before calling this adapter.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"text/template"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/macros"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// ── Config types ──────────────────────────────────────────────────────────────

// publisherKeys is one Thrad Publisher's production/staging key pair.
type publisherKeys struct {
	ProductionKey string `json:"productionKey"`
	StagingKey    string `json:"stagingKey"`
}

// extraInfo is parsed from pbs.yaml adapters.thrad.extra_info (JSON string).
// The embedded publisherKeys is the default/fallback pair (used for any imp
// without a publisherId, or whose publisherId isn't in Publishers); Publishers
// holds the real per-publisher overrides, keyed by Publisher.slug.
type extraInfo struct {
	publisherKeys
	Publishers map[string]publisherKeys `json:"publishers"`
}

// ── Thrad API types ───────────────────────────────────────────────────────────

// ThradBidRequest is the payload sent to POST /api/v1/ssp/bid-request.
type ThradBidRequest struct {
	UserID       string                        `json:"userId"`
	ChatID       string                        `json:"chatId,omitempty"`
	Messages     []openrtb_ext.ExtImpThradsMsg `json:"messages,omitempty"`
	Summary      string                        `json:"summary,omitempty"`
	TurnNumber   *int                          `json:"turn_number,omitempty"`
	Config       *openrtb_ext.ExtImpThradsConfig `json:"config,omitempty"`
	UserMetadata *openrtb_ext.ExtImpThradsUserMeta `json:"user_metadata,omitempty"`
	RequestType  string                        `json:"request_type,omitempty"`
	AdFormats    []string                      `json:"ad_formats,omitempty"`
	Force        bool                          `json:"force,omitempty"`
}

// ThradBidResponse is the top-level Thrad API response envelope.
type ThradBidResponse struct {
	RequestID string      `json:"requestId"`
	Timestamp string      `json:"timestamp"`
	TotalTime float64     `json:"totalTime"`
	Status    string      `json:"status"`
	Message   string      `json:"message"`
	Data      *ThradData  `json:"data"`
	Error     interface{} `json:"error"`
}

// ThradData holds the bid (nil means no-bid).
type ThradData struct {
	Bid *ThradBid `json:"bid"`
}

// ThradBid is the winning creative returned by Thrad.
type ThradBid struct {
	AdFormat    string  `json:"ad_format"`
	Price       float64 `json:"price"`
	Advertiser  string  `json:"advertiser"`
	Domain      string  `json:"domain"`
	Headline    string  `json:"headline"`
	Description string  `json:"description"`
	CTAText     string  `json:"cta_text"`
	// URL is both the destination (redirect-through) and the click tracker —
	// Thrad does not send a separate click-tracking beacon.
	URL         string  `json:"url"`
	Placement   string  `json:"placement"` // "text" or "image"
	LogoURL     string  `json:"logo_url"`
	ImageURL    string  `json:"image_url"`
	// ViewURL is not currently sent by Thrad as a top-level field despite the
	// field name in their docs — impression-tracking pixels instead arrive as
	// view_url/thrad_view_url query params on URL. Kept as a fallback in case
	// Thrad starts sending it directly. See viewTrackerURLs.
	ViewURL     string  `json:"view_url"`
	DSP         string  `json:"dsp"`
	BidID       string  `json:"bidId"`
}

// viewTrackerURLs extracts Thrad's impression-tracking pixel URLs. Thrad
// embeds these as query params (view_url, thrad_view_url — DSP-side and
// SSP-side accounting respectively) on the click/destination URL rather than
// as top-level response fields.
func viewTrackerURLs(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	q := u.Query()
	var trackers []string
	for _, param := range []string{"view_url", "thrad_view_url"} {
		if v := q.Get(param); v != "" {
			trackers = append(trackers, v)
		}
	}
	return trackers
}

// ── Adapter ───────────────────────────────────────────────────────────────────

type adapter struct {
	endpoint *template.Template
	keys     extraInfo
}

// Builder is the constructor registered with PBS.
func Builder(bidderName openrtb_ext.BidderName, cfg config.Adapter, server config.Server) (adapters.Bidder, error) {
	tmpl, err := template.New("endpointTemplate").Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("thrad: unable to parse endpoint template: %w", err)
	}

	a := &adapter{endpoint: tmpl}

	if cfg.ExtraAdapterInfo != "" {
		if err := json.Unmarshal([]byte(cfg.ExtraAdapterInfo), &a.keys); err != nil {
			return nil, fmt.Errorf("thrad: unable to parse extra_info: %w", err)
		}
	}
	if a.keys.ProductionKey == "" && a.keys.StagingKey == "" && len(a.keys.Publishers) == 0 {
		return nil, fmt.Errorf("thrad: extra_info must contain productionKey/stagingKey and/or a publishers map")
	}

	return a, nil
}

// selectKey returns the API key for a given request and publisher. publisherID
// comes from the imp's ExtImpThrads.PublisherID (empty for imps not yet
// migrated to the per-publisher scheme). Falls back to the default/embedded
// key pair when publisherID is empty or has no entry in a.keys.Publishers.
// Within whichever pair is selected, uses stagingKey when test=1 or when no
// productionKey is configured yet.
func (a *adapter) selectKey(request *openrtb2.BidRequest, publisherID string) string {
	keys := a.keys.publisherKeys
	if publisherID != "" {
		if pk, ok := a.keys.Publishers[publisherID]; ok {
			keys = pk
		}
	}
	if request.Test == 1 && keys.StagingKey != "" {
		return keys.StagingKey
	}
	if keys.ProductionKey != "" {
		return keys.ProductionKey
	}
	return keys.StagingKey
}

// MakeRequests translates the OpenRTB auction into one Thrad API call per imp.
func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var requests []*adapters.RequestData
	var errs []error

	for i := range request.Imp {
		imp := request.Imp[i]

		var bidderExt adapters.ExtImpBidder
		if err := json.Unmarshal(imp.Ext, &bidderExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("thrad: invalid imp.ext for imp %s: %s", imp.ID, err),
			})
			continue
		}

		var thradsExt openrtb_ext.ExtImpThrads
		if err := json.Unmarshal(bidderExt.Bidder, &thradsExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("thrad: invalid imp.ext.bidder for imp %s: %s", imp.ID, err),
			})
			continue
		}

		if thradsExt.UserID == "" {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("thrad: userId is required for imp %s (set via window.tpc.data.userId)", imp.ID),
			})
			continue
		}

		// Selected per-imp (not once per BidRequest) so each imp's own
		// publisherId picks the right Thrad Publisher's key.
		apiKey := a.selectKey(request, thradsExt.PublisherID)

		thradReq := ThradBidRequest{
			UserID:     thradsExt.UserID,
			ChatID:     thradsExt.ChatID,
			Messages:   thradsExt.Messages,
			Summary:    thradsExt.Summary,
			TurnNumber: thradsExt.TurnNumber,
			Config:     thradsExt.Config,
		}

		thradReq.RequestType = thradsExt.RequestType
		if thradReq.RequestType == "" {
			thradReq.RequestType = "contextual"
		}

		thradReq.AdFormats = thradsExt.AdFormats
		if len(thradReq.AdFormats) == 0 {
			thradReq.AdFormats = []string{"sponsored_message"}
		}

		// Populate user_metadata from OpenRTB consent signals when not set by publisher.
		if thradsExt.UserMeta != nil {
			thradReq.UserMetadata = thradsExt.UserMeta
		} else {
			meta := &openrtb_ext.ExtImpThradsUserMeta{}
			if request.Regs != nil && request.Regs.USPrivacy != "" {
				meta.USPrivacy = request.Regs.USPrivacy
			}
			if request.User != nil && len(request.User.Ext) > 0 {
				var userExt map[string]json.RawMessage
				if json.Unmarshal(request.User.Ext, &userExt) == nil {
					if consent, ok := userExt["consent"]; ok {
						var s string
						if json.Unmarshal(consent, &s) == nil {
							meta.TCFConsent = s
						}
					}
				}
			}
			if meta.AgeRange != "" || meta.TCFConsent != "" || meta.USPrivacy != "" {
				thradReq.UserMetadata = meta
			}
		}

		body, err := json.Marshal(thradReq)
		if err != nil {
			errs = append(errs, fmt.Errorf("thrad: failed to marshal request for imp %s: %w", imp.ID, err))
			continue
		}

		endpointStr, err := macros.ResolveMacros(a.endpoint, macros.EndpointTemplateParams{})
		if err != nil {
			errs = append(errs, fmt.Errorf("thrad: failed to resolve endpoint: %w", err))
			continue
		}

		headers := http.Header{}
		headers.Set("Content-Type", "application/json;charset=utf-8")
		headers.Set("Accept", "application/json")
		headers.Set("thrad-api-key", apiKey)

		if request.Device != nil {
			if request.Device.IP != "" {
				headers.Set("X-Forwarded-For", request.Device.IP)
			} else if request.Device.IPv6 != "" {
				headers.Set("X-Forwarded-For", request.Device.IPv6)
			}
			if request.Device.UA != "" {
				headers.Set("User-Agent", request.Device.UA)
			}
			if request.Device.Geo != nil && request.Device.Geo.Country != "" {
				headers.Set("X-User-Country", request.Device.Geo.Country)
				switch request.Device.DeviceType {
				case 1, 4, 5: // phone, tablet, connected device
					headers.Set("X-User-Device", "mobile")
				default:
					headers.Set("X-User-Device", "desktop")
				}
				if request.Device.Geo.Region != "" {
					headers.Set("X-User-Region", request.Device.Geo.Region)
				}
				if request.Device.Geo.City != "" {
					headers.Set("X-User-City", request.Device.Geo.City)
				}
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

// MakeBids translates the Thrad API response back into an OpenRTB BidderResponse.
func (a *adapter) MakeBids(request *openrtb2.BidRequest, _ *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode == http.StatusBadRequest {
		return nil, []error{&errortypes.BadInput{
			Message: fmt.Sprintf("thrad: unexpected status code %d: %s", response.StatusCode, response.Body),
		}}
	}
	if response.StatusCode != http.StatusOK {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("thrad: unexpected status code %d: %s", response.StatusCode, response.Body),
		}}
	}

	var thradResp ThradBidResponse
	if err := json.Unmarshal(response.Body, &thradResp); err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("thrad: failed to parse response: %s", err),
		}}
	}

	// status=success with bid=null is a legitimate no-bid, not an error.
	if thradResp.Data == nil || thradResp.Data.Bid == nil {
		return nil, nil
	}

	bid := thradResp.Data.Bid

	if len(request.Imp) == 0 {
		return nil, []error{&errortypes.BadServerResponse{
			Message: "thrad: got bid but request had no imps",
		}}
	}
	imp := request.Imp[0]

	nativeAdm, err := buildNativeAdm(bid)
	if err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("thrad: failed to build native adm: %s", err),
		}}
	}

	bidID := bid.BidID
	if bidID == "" {
		bidID = thradResp.RequestID
	}

	var adomain []string
	if bid.Domain != "" {
		adomain = []string{bid.Domain}
	}

	var burl string
	if trackers := viewTrackerURLs(bid.URL); len(trackers) > 0 {
		burl = trackers[0]
	} else {
		burl = bid.ViewURL
	}

	ortbBid := openrtb2.Bid{
		ID:      bidID,
		ImpID:   imp.ID,
		Price:   bid.Price,
		AdM:     nativeAdm,
		BURL:    burl, // note: not currently fired by this PBS fork — see buildNativeAdm's imptrackers for the mechanism that actually fires
		ADomain: adomain,
		CrID:    bid.BidID,
	}

	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(1)
	bidderResponse.Bids = append(bidderResponse.Bids, &adapters.TypedBid{
		Bid:     &ortbBid,
		BidType: openrtb_ext.BidTypeNative,
	})
	bidderResponse.Currency = "USD"

	return bidderResponse, nil
}

// buildNativeAdm constructs an OpenRTB native adm string from a Thrad bid.
//
// Asset IDs are 0-indexed to match what Prebid.js generates from the legacy
// native ad unit definition (title→0, image→1, body→2, sponsoredBy→3).
// Prebid.js validates bid response assets against the IDs it sent in the
// request, so these must align or required-asset checks fail.
//
//	0 = title        (required)
//	1 = img          (main image if available, logo as fallback; optional)
//	2 = data/desc    (description/body)
//	3 = data/sponsor (advertiser/sponsoredBy)
//	4 = data/cta     (cta text)
func buildNativeAdm(bid *ThradBid) (string, error) {
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
		{ID: 0, Title: &nativeTitle{Text: bid.Headline}},
	}

	// id 1 = img slot: prefer main image (type 3), fall back to logo (type 2)
	if bid.ImageURL != "" {
		assets = append(assets, nativeAsset{ID: 1, Img: &nativeImg{URL: bid.ImageURL, Type: 3}})
	} else if bid.LogoURL != "" {
		assets = append(assets, nativeAsset{ID: 1, Img: &nativeImg{URL: bid.LogoURL, Type: 2}})
	}

	if bid.Description != "" {
		assets = append(assets, nativeAsset{ID: 2, Data: &nativeData{Value: bid.Description}})
	}

	if bid.Advertiser != "" {
		assets = append(assets, nativeAsset{ID: 3, Data: &nativeData{Value: bid.Advertiser}})
	}

	ctaText := bid.CTAText
	if ctaText == "" {
		ctaText = "Learn More"
	}
	assets = append(assets, nativeAsset{ID: 4, Data: &nativeData{Value: ctaText}})

	adm := nativeAdmWrapper{
		Ver:    "1.1",
		Link:   nativeLink{URL: bid.URL},
		Assets: assets,
	}
	if trackers := viewTrackerURLs(bid.URL); len(trackers) > 0 {
		adm.ImpTrackers = trackers
	} else if bid.ViewURL != "" {
		adm.ImpTrackers = []string{bid.ViewURL}
	}

	b, err := json.Marshal(adm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
