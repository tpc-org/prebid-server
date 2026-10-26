package thrad

// ── Protocol v1 (ssp.thrads.ai/api/v1/ssp/bid-request) ─────────────────────────
//
// Retired by Thrad on 2026-11-04. Kept only so placements can move to v2 one
// at a time (see thrad.go); delete this file once every placement is on v2.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/prebid/openrtb/v20/adcom1"
	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// ── Thrad API types ───────────────────────────────────────────────────────────

// ThradBidRequest is the payload sent to POST /api/v1/ssp/bid-request.
type ThradBidRequest struct {
	UserID       string                            `json:"userId"`
	ChatID       string                            `json:"chatId,omitempty"`
	Messages     []openrtb_ext.ExtImpThradsMsg     `json:"messages,omitempty"`
	Summary      string                            `json:"summary,omitempty"`
	TurnNumber   *int                              `json:"turn_number,omitempty"`
	Config       *openrtb_ext.ExtImpThradsConfig   `json:"config,omitempty"`
	UserMetadata *openrtb_ext.ExtImpThradsUserMeta `json:"user_metadata,omitempty"`
	RequestType  string                            `json:"request_type,omitempty"`
	AdFormats    []string                          `json:"ad_formats,omitempty"`
	Force        bool                              `json:"force,omitempty"`
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
	URL       string `json:"url"`
	Placement string `json:"placement"` // "text" or "image"
	LogoURL   string `json:"logo_url"`
	ImageURL  string `json:"image_url"`
	// ViewURL is not currently sent by Thrad as a top-level field despite the
	// field name in their docs — impression-tracking pixels instead arrive as
	// view_url/thrad_view_url query params on URL. Kept as a fallback in case
	// Thrad starts sending it directly. See viewTrackerURLs.
	ViewURL string `json:"view_url"`
	DSP     string `json:"dsp"`
	BidID   string `json:"bidId"`
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

// makeRequestV1 builds one v1 call for one imp.
func (a *adapter) makeRequestV1(request *openrtb2.BidRequest, imp openrtb2.Imp, thradsExt openrtb_ext.ExtImpThrads) (*adapters.RequestData, error) {
	if thradsExt.UserID == "" {
		return nil, &errortypes.BadInput{
			Message: fmt.Sprintf("thrad: userId is required for imp %s (set via window.tpc.data.userId)", imp.ID),
		}
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
		return nil, fmt.Errorf("thrad: failed to marshal request for imp %s: %w", imp.ID, err)
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
		// Device class is sent whenever it's known, not only alongside a
		// geo country as before: app traffic (e.g. Gravity resale) often
		// has no device.geo, and previously got no device signal at all.
		if class := userDeviceClass(request.Device.DeviceType, request.Device.UA); class != "" {
			headers.Set("X-User-Device", class)
		}
		if geo := request.Device.Geo; geo != nil {
			if country := alpha2Country(geo.Country); country != "" {
				headers.Set("X-User-Country", country)
			}
			if geo.Region != "" {
				headers.Set("X-User-Region", geo.Region)
			}
			if geo.City != "" {
				headers.Set("X-User-City", geo.City)
			}
		}
	}

	return &adapters.RequestData{
		Method:  "POST",
		Uri:     a.v1Endpoint,
		Body:    body,
		Headers: headers,
		ImpIDs:  []string{imp.ID},
	}, nil
}

// userDeviceClass maps OpenRTB devicetype onto Thrad v1's X-User-Device
// ("mobile" or "desktop"), falling back to the user agent when devicetype is
// unset. Returns "" when unknown rather than guessing "desktop" (the previous
// default, which mislabelled app traffic without a devicetype). Tablets count
// as mobile; CTV/set-top boxes have no v1 equivalent.
func userDeviceClass(dt adcom1.DeviceType, ua string) string {
	switch dt {
	case adcom1.DeviceMobile, adcom1.DevicePhone, adcom1.DeviceTablet:
		return "mobile"
	case adcom1.DevicePC:
		return "desktop"
	case adcom1.DeviceTV, adcom1.DeviceConnected, adcom1.DeviceSetTopBox:
		return ""
	}
	switch {
	case ua == "":
		return ""
	case strings.Contains(ua, "Mobi") || strings.Contains(ua, "Android") ||
		strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad") || strings.Contains(ua, "iPod"):
		return "mobile"
	case strings.Contains(ua, "Windows") || strings.Contains(ua, "Macintosh") ||
		strings.Contains(ua, "CrOS") || strings.Contains(ua, "X11"):
		return "desktop"
	}
	return ""
}

// makeBidsV1 translates a v1 response back into an OpenRTB BidderResponse.
func (a *adapter) makeBidsV1(request *openrtb2.BidRequest, requestData *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
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

	// One imp per call (see makeRequestV1), so the call's own imp is the one
	// that was bid on — not request.Imp[0], which was wrong for multi-imp
	// requests.
	impID := callImpID(request, requestData)
	if impID == "" {
		return nil, []error{&errortypes.BadServerResponse{
			Message: "thrad: got bid but can't tell which imp it is for",
		}}
	}

	nativeAdm, err := buildNativeAdmV1(bid)
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
		ImpID:   impID,
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

// buildNativeAdmV1 constructs an OpenRTB native adm string from a Thrad bid.
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
func buildNativeAdmV1(bid *ThradBid) (string, error) {
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
