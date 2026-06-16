package thrad

// Package thrad implements a Prebid Server adapter for Thrad (thrads.ai).
//
// ── Key selection (staging vs production) ────────────────────────────────────
//
// Thrad issues two API keys per publisher: one for staging (always returns
// test ads, no monetisation) and one for production.
//
// Keys are configured in pbs-settings/pbs.yaml under adapters.thrads.extra_info:
//
//	adapters:
//	  thrads:
//	    endpoint: "https://ssp.thrads.ai/api/v1/ssp/bid-request"
//	    extra_info: '{"productionKey":"pk_live_xxx","stagingKey":"pk_staging_yyy"}'
//
// When the incoming OpenRTB request carries BidRequest.Test = 1
// (set automatically by Prebid.js when ?pbjs_debug=true), the adapter
// uses the stagingKey. Otherwise it uses the productionKey.
//
// ── Conversational context ───────────────────────────────────────────────────
//
// Thrad requires userId, chatId, and messages per auction.
// These are dynamic (change every auction) and cannot live in a Stored Imp.
// Publishers pass them via window.tpc.data; the sayhola bundle reads them
// and injects them into params.thrads, which tpcBidAdapter forwards as
// imp.ext.prebid.bidder.thrads in the OpenRTB request.
// PBS merges that with the Stored Imp's static thrads params before calling
// this adapter.

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

// extraInfo is parsed from pbs.yaml adapters.thrads.extra_info (JSON string).
type extraInfo struct {
	ProductionKey string `json:"productionKey"`
	StagingKey    string `json:"stagingKey"`
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
	URL         string  `json:"url"`      // click tracking URL
	Placement   string  `json:"placement"` // "text" or "image"
	LogoURL     string  `json:"logo_url"`
	ImageURL    string  `json:"image_url"`
	ViewURL     string  `json:"view_url"` // impression pixel (viewability billing)
	DSP         string  `json:"dsp"`
	BidID       string  `json:"bidId"`
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
		return nil, fmt.Errorf("thrads: unable to parse endpoint template: %w", err)
	}

	a := &adapter{endpoint: tmpl}

	if cfg.ExtraAdapterInfo != "" {
		if err := json.Unmarshal([]byte(cfg.ExtraAdapterInfo), &a.keys); err != nil {
			return nil, fmt.Errorf("thrads: unable to parse extra_info: %w", err)
		}
	}
	if a.keys.ProductionKey == "" && a.keys.StagingKey == "" {
		return nil, fmt.Errorf("thrads: extra_info must contain productionKey and/or stagingKey")
	}

	return a, nil
}

// selectKey returns the API key for a given request.
// Uses stagingKey when test=1 or when no productionKey is configured yet.
func (a *adapter) selectKey(request *openrtb2.BidRequest) string {
	if request.Test == 1 && a.keys.StagingKey != "" {
		return a.keys.StagingKey
	}
	if a.keys.ProductionKey != "" {
		return a.keys.ProductionKey
	}
	return a.keys.StagingKey
}

// MakeRequests translates the OpenRTB auction into one Thrad API call per imp.
func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var requests []*adapters.RequestData
	var errs []error

	apiKey := a.selectKey(request)

	for i := range request.Imp {
		imp := request.Imp[i]

		var bidderExt adapters.ExtImpBidder
		if err := json.Unmarshal(imp.Ext, &bidderExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("thrads: invalid imp.ext for imp %s: %s", imp.ID, err),
			})
			continue
		}

		var thradsExt openrtb_ext.ExtImpThrads
		if err := json.Unmarshal(bidderExt.Bidder, &thradsExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("thrads: invalid imp.ext.bidder for imp %s: %s", imp.ID, err),
			})
			continue
		}

		if thradsExt.UserID == "" {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("thrads: userId is required for imp %s (set via window.tpc.data.userId)", imp.ID),
			})
			continue
		}

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
			errs = append(errs, fmt.Errorf("thrads: failed to marshal request for imp %s: %w", imp.ID, err))
			continue
		}

		endpointStr, err := macros.ResolveMacros(a.endpoint, macros.EndpointTemplateParams{})
		if err != nil {
			errs = append(errs, fmt.Errorf("thrads: failed to resolve endpoint: %w", err))
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
			Message: fmt.Sprintf("thrads: unexpected status code %d: %s", response.StatusCode, response.Body),
		}}
	}
	if response.StatusCode != http.StatusOK {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("thrads: unexpected status code %d: %s", response.StatusCode, response.Body),
		}}
	}

	var thradResp ThradBidResponse
	if err := json.Unmarshal(response.Body, &thradResp); err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("thrads: failed to parse response: %s", err),
		}}
	}

	// status=success with bid=null is a legitimate no-bid, not an error.
	if thradResp.Data == nil || thradResp.Data.Bid == nil {
		return nil, nil
	}

	bid := thradResp.Data.Bid

	if len(request.Imp) == 0 {
		return nil, []error{&errortypes.BadServerResponse{
			Message: "thrads: got bid but request had no imps",
		}}
	}
	imp := request.Imp[0]

	nativeAdm, err := buildNativeAdm(bid)
	if err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("thrads: failed to build native adm: %s", err),
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

	ortbBid := openrtb2.Bid{
		ID:      bidID,
		ImpID:   imp.ID,
		Price:   bid.Price,
		AdM:     nativeAdm,
		BURL:    bid.ViewURL, // PBS fires as billing notice after win
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
// Asset IDs match the Stored Imp native.request assets (sayhola-9243e9b6.json):
//
//	1 = title        (required)
//	2 = img          (main image if available, logo as fallback; optional)
//	3 = data/desc    (description)
//	4 = data/sponsor (advertiser name)
//	5 = data/cta     (cta_text — extra, not in stored imp, harmless)
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
		{ID: 1, Title: &nativeTitle{Text: bid.Headline}},
	}

	// id 2 = img slot: prefer main image (type 3), fall back to logo (type 2)
	if bid.ImageURL != "" {
		assets = append(assets, nativeAsset{ID: 2, Img: &nativeImg{URL: bid.ImageURL, Type: 3}})
	} else if bid.LogoURL != "" {
		assets = append(assets, nativeAsset{ID: 2, Img: &nativeImg{URL: bid.LogoURL, Type: 2}})
	}

	if bid.Description != "" {
		assets = append(assets, nativeAsset{ID: 3, Data: &nativeData{Value: bid.Description}})
	}

	if bid.Advertiser != "" {
		assets = append(assets, nativeAsset{ID: 4, Data: &nativeData{Value: bid.Advertiser}})
	}

	ctaText := bid.CTAText
	if ctaText == "" {
		ctaText = "Learn More"
	}
	assets = append(assets, nativeAsset{ID: 5, Data: &nativeData{Value: ctaText}})

	adm := nativeAdmWrapper{
		Ver:    "1.1",
		Link:   nativeLink{URL: bid.URL},
		Assets: assets,
	}
	if bid.ViewURL != "" {
		adm.ImpTrackers = []string{bid.ViewURL}
	}

	b, err := json.Marshal(adm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
