// Package adgentek implements the Prebid Server adapter for Adgentek
// (api.adgentek.ai), a REST ad server for AI conversations (HOL-157).
//
// Contract: "REST API", docs.adgentek.ai/publishers/integrations/rest-api
// (copy in the brain's Drive, 2026-11-22). Adgentek publishes no OpenRTB
// endpoint and no machine-readable contract for ad serving.
//
// Configuration lives in pbs-settings/pbs.yaml:
//
//	adapters:
//	  adgentek:
//	    disabled: false
//	    extra_info: '{"apiKey":"..."}'
//
// The key is our publisher key, sent as x-mcp-api-key. The bidder is disabled
// in static/bidder-info/adgentek.yaml, and Builder fails (PBS will not start)
// without the key: enable the bidder and add extra_info in the same release.
//
// # Request
//
// Each native imp becomes one POST <endpoint>:
//
//		{"slot_id": params.slotId,
//		 "context": {"messages": [...], "session_id": "..."},
//		 "surface_type": "conversational",
//		 "format_preferences": ["native"],
//		 "geo": {"country": "US", ...},
//		 "custom": {...}}
//
//	  - slot_id is the placement id Adgentek minted for the publisher, from the
//	    Stored Imp (or dsp-connector's per-site params).
//	  - messages are required by Adgentek. They come from params.messages
//	    (dsp-connector) or else from bidder-scoped first-party data,
//	    site.ext.data.messages (our bundles, the same channel Kontext reads).
//	    Only user/assistant/system turns with text are sent, the last
//	    maxMessages of them.
//	  - session_id (frequency capping) comes from params.sessionId, else
//	    site.ext.data.sessionId. Without one it is left out.
//	  - geo.country is device.geo.country converted from OpenRTB's alpha-3 to
//	    alpha-2 (PBS's activitylog module fills it from the user's IP). Region,
//	    city and zip are passed as they are. Our call comes from a datacenter,
//	    so without this Adgentek would target our server's location.
//
// An imp without a slot id or any conversation is skipped with a soft error
// and never sent. Child-directed requests (regs.coppa = 1) are not sent.
// Unknown fields are ignored by Adgentek, not rejected, so a typo here would
// fail silently: the field names above are checked against the spec in the
// tests.
//
// # Response
//
// {"ads": [...]} with an empty list is a no-fill. The first ad becomes one
// native bid for the imp:
//
//   - price is bid_price, a CPM in USD (the cleared price we are credited);
//   - the native markup is built against the imp's own native request, so it
//     carries the caller's asset ids: title, main image, description (the
//     ad's content), sponsored-by (the advertiser domain) and CTA, each cut to
//     the requested length;
//   - link.url is click_url, which must be used as is: Adgentek's click
//     filtering and CPC billing run on its redirect;
//   - impression_url is the one and only tracker, an event 1 image tracker.
//     Adgentek bills only for a pixel fired from the user's browser as an
//     image request, never server-side (that is GIVT and is discarded). Our
//     bundles fire it from the browser via Prebid core; a reseller (through
//     dsp-connector) receives it as its first impression tracker and fires it
//     client-side. There must be no second tracker: dsp-connector fires extra
//     impression trackers itself, from the server.
package adgentek

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/prebid/openrtb/v20/native1"
	nativeRequest "github.com/prebid/openrtb/v20/native1/request"
	nativeResponse "github.com/prebid/openrtb/v20/native1/response"
	"github.com/prebid/openrtb/v20/openrtb2"
	"golang.org/x/text/language"

	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/prebid/prebid-server/v4/util/jsonutil"
)

const (
	defaultSurfaceType = "conversational"
	maxMessages        = 10
)

type extraInfo struct {
	APIKey string `json:"apiKey"`
}

type adapter struct {
	endpoint string
	apiKey   string
}

// Builder builds the Adgentek adapter. It is only called when the bidder is
// enabled, and fails without an endpoint and an API key.
func Builder(_ openrtb_ext.BidderName, cfg config.Adapter, _ config.Server) (adapters.Bidder, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("adgentek: endpoint is not configured")
	}
	if cfg.ExtraAdapterInfo == "" {
		return nil, errors.New("adgentek: extra_info with apiKey is required")
	}
	var info extraInfo
	if err := jsonutil.Unmarshal([]byte(cfg.ExtraAdapterInfo), &info); err != nil {
		return nil, fmt.Errorf("adgentek: invalid extra_info: %w", err)
	}
	if strings.TrimSpace(info.APIKey) == "" {
		return nil, errors.New("adgentek: extra_info must set apiKey")
	}
	return &adapter{endpoint: cfg.Endpoint, apiKey: info.APIKey}, nil
}

// getAdRequest is Adgentek's POST /ad-server/v1/get-ad body.
type getAdRequest struct {
	SlotID            string            `json:"slot_id"`
	Context           getAdContext      `json:"context"`
	SurfaceType       string            `json:"surface_type,omitempty"`
	FormatPreferences []string          `json:"format_preferences,omitempty"`
	Geo               *getAdGeo         `json:"geo,omitempty"`
	Custom            map[string]string `json:"custom,omitempty"`
}

type getAdContext struct {
	Messages  []openrtb_ext.AdgentekMessage `json:"messages"`
	SessionID string                        `json:"session_id,omitempty"`
}

type getAdGeo struct {
	Country string `json:"country,omitempty"`
	Region  string `json:"region,omitempty"`
	City    string `json:"city,omitempty"`
	Zip     string `json:"zip,omitempty"`
}

// siteExt is the part of site.ext (bidder-scoped first-party data) the
// adapter reads.
type siteExt struct {
	Data struct {
		Messages  []openrtb_ext.AdgentekMessage `json:"messages"`
		SessionID string                        `json:"sessionId"`
	} `json:"data"`
}

func (a *adapter) MakeRequests(request *openrtb2.BidRequest, _ *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	if request.Regs != nil && request.Regs.COPPA == 1 {
		return nil, []error{&errortypes.BadInput{Message: "adgentek: child-directed requests are not sent"}}
	}

	var fpd siteExt
	if request.Site != nil && len(request.Site.Ext) > 0 {
		// Unreadable first-party data is not fatal: params may still carry
		// the conversation.
		_ = jsonutil.Unmarshal(request.Site.Ext, &fpd)
	}
	geo := buildGeo(request.Device)

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("x-mcp-api-key", a.apiKey)

	var errs []error
	var out []*adapters.RequestData
	for i := range request.Imp {
		imp := &request.Imp[i]
		body, err := buildBody(imp, fpd, geo)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		raw, err := marshalPlain(body)
		if err != nil {
			errs = append(errs, fmt.Errorf("adgentek: failed to marshal request for imp %s: %w", imp.ID, err))
			continue
		}
		out = append(out, &adapters.RequestData{
			Method:  http.MethodPost,
			Uri:     a.endpoint,
			Body:    raw,
			Headers: headers.Clone(),
			ImpIDs:  []string{imp.ID},
		})
	}
	return out, errs
}

func buildBody(imp *openrtb2.Imp, fpd siteExt, geo *getAdGeo) (*getAdRequest, error) {
	if imp.Native == nil {
		return nil, &errortypes.BadInput{Message: fmt.Sprintf("adgentek: imp %s has no native object", imp.ID)}
	}
	var ext adapters.ExtImpBidder
	if err := jsonutil.Unmarshal(imp.Ext, &ext); err != nil {
		return nil, &errortypes.BadInput{Message: fmt.Sprintf("adgentek: invalid imp.ext for imp %s: %s", imp.ID, err)}
	}
	var params openrtb_ext.ImpExtAdgentek
	if err := jsonutil.Unmarshal(ext.Bidder, &params); err != nil {
		return nil, &errortypes.BadInput{Message: fmt.Sprintf("adgentek: invalid imp.ext.bidder for imp %s: %s", imp.ID, err)}
	}
	slotID := strings.TrimSpace(params.SlotID)
	if slotID == "" {
		return nil, &errortypes.BadInput{Message: fmt.Sprintf("adgentek: imp %s has no slotId", imp.ID)}
	}

	messages := cleanMessages(params.Messages)
	if len(messages) == 0 {
		messages = cleanMessages(fpd.Data.Messages)
	}
	if len(messages) == 0 {
		// Adgentek requires context.messages. An auction before the first
		// user turn has nothing to monetise.
		return nil, &errortypes.BadInput{Message: fmt.Sprintf("adgentek: imp %s has no conversation messages", imp.ID)}
	}
	session := strings.TrimSpace(params.SessionID)
	if session == "" {
		session = strings.TrimSpace(fpd.Data.SessionID)
	}
	surface := params.SurfaceType
	if surface == "" {
		surface = defaultSurfaceType
	}

	return &getAdRequest{
		SlotID:            slotID,
		Context:           getAdContext{Messages: messages, SessionID: session},
		SurfaceType:       surface,
		FormatPreferences: []string{"native"},
		Geo:               geo,
		Custom:            params.Custom,
	}, nil
}

// cleanMessages keeps user, assistant and system turns that have text, the
// most recent maxMessages of them, oldest first.
func cleanMessages(in []openrtb_ext.AdgentekMessage) []openrtb_ext.AdgentekMessage {
	out := make([]openrtb_ext.AdgentekMessage, 0, len(in))
	for _, m := range in {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if (role == "user" || role == "assistant" || role == "system") && strings.TrimSpace(m.Content) != "" {
			out = append(out, openrtb_ext.AdgentekMessage{Role: role, Content: m.Content})
		}
	}
	if len(out) > maxMessages {
		out = out[len(out)-maxMessages:]
	}
	return out
}

func buildGeo(device *openrtb2.Device) *getAdGeo {
	if device == nil || device.Geo == nil {
		return nil
	}
	g := &getAdGeo{
		Country: alpha2Country(device.Geo.Country),
		Region:  device.Geo.Region,
		City:    device.Geo.City,
		Zip:     device.Geo.ZIP,
	}
	if *g == (getAdGeo{}) {
		return nil
	}
	return g
}

// alpha2Country converts an ISO-3166-1 country code (alpha-3 as OpenRTB
// specifies, or alpha-2) to alpha-2, or "" when it is not a real country.
func alpha2Country(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	region, err := language.ParseRegion(code)
	if err != nil || !region.IsCountry() {
		return ""
	}
	return region.String()
}

// getAdResponse is Adgentek's reply. Only the fields the bid uses are read.
type getAdResponse struct {
	Ads []adgentekAd `json:"ads"`
}

type adgentekAd struct {
	ID               string   `json:"id"`
	CreativeID       string   `json:"creative_id"`
	Format           string   `json:"format"`
	Title            string   `json:"title"`
	Content          string   `json:"content"`
	CTA              string   `json:"cta"`
	ImageURL         string   `json:"image_url"`
	ClickURL         string   `json:"click_url"`
	ImpressionURL    string   `json:"impression_url"`
	BidPrice         float64  `json:"bid_price"`
	ClearedPriceUSD  float64  `json:"cleared_price_usd"`
	AdvertiserDomain string   `json:"advertiser_domain"`
	ADomain          []string `json:"adomain"`
}

func (a *adapter) MakeBids(request *openrtb2.BidRequest, requestData *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if adapters.IsResponseStatusCodeNoContent(response) {
		return nil, nil
	}
	switch {
	case response.StatusCode == http.StatusBadRequest:
		return nil, []error{&errortypes.BadInput{Message: "adgentek: request rejected as malformed (400)"}}
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, []error{&errortypes.BadServerResponse{Message: fmt.Sprintf("adgentek: publisher key rejected (%d)", response.StatusCode)}}
	case response.StatusCode != http.StatusOK:
		return nil, []error{&errortypes.BadServerResponse{Message: fmt.Sprintf("adgentek: unexpected status code %d", response.StatusCode)}}
	}

	var resp getAdResponse
	if err := jsonutil.Unmarshal(response.Body, &resp); err != nil {
		return nil, []error{&errortypes.BadServerResponse{Message: fmt.Sprintf("adgentek: failed to parse response: %s", err)}}
	}
	if len(resp.Ads) == 0 {
		// No-fill is the normal outcome on conversational inventory.
		return nil, nil
	}
	// Each call carries exactly one imp (MakeRequests sets ImpIDs). Without
	// ImpIDs, only a single-imp request can be mapped.
	var impID string
	switch {
	case requestData != nil && len(requestData.ImpIDs) == 1:
		impID = requestData.ImpIDs[0]
	case len(request.Imp) == 1:
		impID = request.Imp[0].ID
	default:
		return nil, []error{&errortypes.BadServerResponse{Message: "adgentek: response does not map to one imp"}}
	}
	imp := findImp(request, impID)
	if imp == nil || imp.Native == nil {
		return nil, []error{&errortypes.BadServerResponse{Message: fmt.Sprintf("adgentek: response for unknown imp %q", impID)}}
	}

	ad := resp.Ads[0]
	bid, err := buildBid(imp, ad)
	if err != nil {
		return nil, []error{err}
	}
	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(1)
	bidderResponse.Currency = "USD"
	typed := &adapters.TypedBid{Bid: bid, BidType: openrtb_ext.BidTypeNative}
	if len(bid.ADomain) > 0 {
		typed.BidMeta = &openrtb_ext.ExtBidPrebidMeta{AdvertiserDomains: bid.ADomain}
	}
	bidderResponse.Bids = append(bidderResponse.Bids, typed)
	return bidderResponse, nil
}

func findImp(request *openrtb2.BidRequest, id string) *openrtb2.Imp {
	for i := range request.Imp {
		if request.Imp[i].ID == id {
			return &request.Imp[i]
		}
	}
	return nil
}

func buildBid(imp *openrtb2.Imp, ad adgentekAd) (*openrtb2.Bid, error) {
	price := ad.BidPrice
	if price <= 0 {
		price = ad.ClearedPriceUSD
	}
	if price <= 0 {
		return nil, &errortypes.BadServerResponse{Message: fmt.Sprintf("adgentek: ad %s has no price", ad.ID)}
	}
	if ad.ClickURL == "" {
		return nil, &errortypes.BadServerResponse{Message: fmt.Sprintf("adgentek: ad %s has no click_url", ad.ID)}
	}
	if !strings.HasPrefix(ad.ImpressionURL, "https://") {
		// Unbillable without the browser pixel.
		return nil, &errortypes.BadServerResponse{Message: fmt.Sprintf("adgentek: ad %s has no https impression_url", ad.ID)}
	}

	adm, err := buildNativeAdm(imp.Native.Request, ad)
	if err != nil {
		return nil, &errortypes.BadServerResponse{Message: fmt.Sprintf("adgentek: ad %s: %s", ad.ID, err)}
	}

	adomain := ad.ADomain
	if len(adomain) == 0 && ad.AdvertiserDomain != "" {
		adomain = []string{ad.AdvertiserDomain}
	}
	id := ad.ID
	if id == "" {
		id = imp.ID
	}
	return &openrtb2.Bid{
		ID:      id,
		ImpID:   imp.ID,
		Price:   price,
		AdM:     adm,
		ADomain: adomain,
		CrID:    ad.CreativeID,
		MType:   openrtb2.MarkupNative,
	}, nil
}

// buildNativeAdm answers the imp's own native request asset by asset, so the
// markup carries the caller's asset ids.
func buildNativeAdm(rawRequest string, ad adgentekAd) (string, error) {
	var req nativeRequest.Request
	if err := jsonutil.Unmarshal([]byte(rawRequest), &req); err != nil {
		return "", fmt.Errorf("imp native request is not valid JSON: %s", err)
	}
	if len(req.Assets) == 0 {
		// Prebid.js 1.1-style wrapper {"native":{...}}.
		var wrapped struct {
			Native nativeRequest.Request `json:"native"`
		}
		if err := jsonutil.Unmarshal([]byte(rawRequest), &wrapped); err == nil {
			req = wrapped.Native
		}
	}
	if len(req.Assets) == 0 {
		return "", errors.New("imp native request has no assets")
	}

	sponsor := ad.AdvertiserDomain
	if sponsor == "" && len(ad.ADomain) > 0 {
		sponsor = ad.ADomain[0]
	}

	resp := nativeResponse.Response{
		Ver:  "1.2",
		Link: nativeResponse.Link{URL: ad.ClickURL},
		EventTrackers: []nativeResponse.EventTracker{{
			Event:  native1.EventTypeImpression,
			Method: native1.EventTrackingMethodImage,
			URL:    ad.ImpressionURL,
		}},
	}
	for _, want := range req.Assets {
		id := want.ID
		var asset *nativeResponse.Asset
		switch {
		case want.Title != nil && ad.Title != "":
			asset = &nativeResponse.Asset{ID: &id, Title: &nativeResponse.Title{Text: truncate(ad.Title, want.Title.Len)}}
		case want.Img != nil && want.Img.Type == native1.ImageAssetTypeMain && ad.ImageURL != "":
			asset = &nativeResponse.Asset{ID: &id, Img: &nativeResponse.Image{Type: native1.ImageAssetTypeMain, URL: ad.ImageURL}}
		case want.Data != nil:
			var value string
			switch want.Data.Type {
			case native1.DataAssetTypeDesc:
				value = ad.Content
			case native1.DataAssetTypeSponsored:
				value = sponsor
			case native1.DataAssetTypeCTAText:
				value = ad.CTA
			}
			if value != "" {
				asset = &nativeResponse.Asset{ID: &id, Data: &nativeResponse.Data{Value: truncate(value, want.Data.Len)}}
			}
		}
		if asset != nil {
			resp.Assets = append(resp.Assets, *asset)
		}
	}
	if len(resp.Assets) == 0 {
		return "", errors.New("no requested asset could be filled")
	}

	out, err := marshalPlain(resp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// truncate cuts s to at most n runes; n <= 0 means no limit. A Native
// request's len is a maximum character count.
func truncate(s string, n int64) string {
	if n <= 0 || int64(utf8.RuneCountInString(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// marshalPlain encodes without HTML escaping, so "&" in a tracking URL stays
// "&" instead of becoming "&".
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
