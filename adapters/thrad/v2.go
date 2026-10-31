package thrad

// ── Protocol v2 (ssp.thrad.ai/api/v2/bid-request) ──────────────────────────────
//
// Thrad's current protocol (docs.thrad.ai/api/api-reference/bid-request-v2),
// validated on the test-only thradx placement before going live.
//
// Request mapping (v1 field → v2):
//
//	chatId              → context.id
//	messages            → context.chat.messages (ISO 8601 timestamps → Unix seconds)
//	turnNumber          → context.chat.turn_number (else: count of user turns)
//	summary             → context.summary
//	userId              → user.id
//	userMetadata        → user.age_range, privacy.tcf / privacy.us_privacy
//	adFormats           → opportunities[0].formats (card only for now)
//	device.ip/ua/geo/…  → device.* (v1 sent these as X-User-* headers)
//	site / app          → surface (omitted unless fully valid — see buildSurface)
//	thrad-api-key       → Authorization: Bearer
//
// v2 rejects unknown fields and many malformed values with a 400 for the
// whole request, so every builder below omits a field rather than send a
// value it isn't sure Thrad will accept. requestType is ignored: v2 infers
// pre-conversation (opener) vs contextual from whether any conversation
// content is present.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/prebid/openrtb/v20/adcom1"
	"github.com/prebid/openrtb/v20/native1"
	"github.com/prebid/openrtb/v20/openrtb2"
	"golang.org/x/text/language"

	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/macros"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// ── Thrad v2 request types ────────────────────────────────────────────────────

type v2Request struct {
	Version       string          `json:"version"`
	ID            string          `json:"id,omitempty"`
	Context       *v2Context      `json:"context,omitempty"`
	Opportunities []v2Opportunity `json:"opportunities"`
	User          *v2User         `json:"user,omitempty"`
	Device        *v2Device       `json:"device,omitempty"`
	Surface       *v2Surface      `json:"surface,omitempty"`
	Privacy       *v2Privacy      `json:"privacy,omitempty"`
}

type v2Context struct {
	ID      string  `json:"id,omitempty"`
	Flow    string  `json:"flow,omitempty"`
	Chat    *v2Chat `json:"chat,omitempty"`
	Summary string  `json:"summary,omitempty"`
}

type v2Chat struct {
	Messages   []v2Message `json:"messages"`
	TurnNumber *int        `json:"turn_number,omitempty"`
}

type v2Message struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp *int64 `json:"timestamp,omitempty"`
}

type v2Opportunity struct {
	ID      string     `json:"id"`
	Formats []v2Format `json:"formats"`
}

type v2Format struct {
	Type   string         `json:"type"`
	Assets v2FormatAssets `json:"assets"`
}

type v2FormatAssets struct {
	Title       *v2AssetSpec `json:"title,omitempty"`
	Description *v2AssetSpec `json:"description,omitempty"`
	Image       *v2AssetSpec `json:"image,omitempty"`
}

// v2AssetSpec is sent as {} when there's no constraint (Thrad's docs: "Send
// image as {}").
type v2AssetSpec struct {
	MaxChars *int `json:"max_chars,omitempty"`
}

type v2User struct {
	ID          string         `json:"id,omitempty"`
	AgeRange    string         `json:"age_range,omitempty"`
	ExtendedIDs *v2ExtendedIDs `json:"extended_ids,omitempty"`
}

type v2ExtendedIDs struct {
	EmailsSHA256 []string `json:"emails_sha256,omitempty"`
}

type v2Device struct {
	Geo       *v2Geo       `json:"geo,omitempty"`
	Type      string       `json:"type,omitempty"`
	OS        string       `json:"os,omitempty"`
	OSVersion string       `json:"os_version,omitempty"`
	Model     string       `json:"model,omitempty"`
	Language  string       `json:"language,omitempty"`
	IP        string       `json:"ip,omitempty"`
	UserAgent string       `json:"user_agent,omitempty"`
	IDs       *v2DeviceIDs `json:"ids,omitempty"`
}

type v2Geo struct {
	Country   string   `json:"country,omitempty"`
	Region    string   `json:"region,omitempty"`
	City      string   `json:"city,omitempty"`
	Zip       string   `json:"zip,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
}

type v2DeviceIDs struct {
	IDFA string `json:"idfa,omitempty"`
	GAID string `json:"gaid,omitempty"`
}

type v2Surface struct {
	Type     string `json:"type"`
	Domain   string `json:"domain,omitempty"`
	Store    string `json:"store,omitempty"`
	Bundle   string `json:"bundle,omitempty"`
	StoreURL string `json:"store_url,omitempty"`
}

type v2Privacy struct {
	GDPRApplies     *bool  `json:"gdpr_applies,omitempty"`
	TCF             string `json:"tcf,omitempty"`
	USPrivacy       string `json:"us_privacy,omitempty"`
	ATTStatus       string `json:"att_status,omitempty"`
	LimitAdTracking *bool  `json:"limit_ad_tracking,omitempty"`
}

// ── Thrad v2 response types ───────────────────────────────────────────────────

type v2Response struct {
	Version       string          `json:"version"`
	ID            string          `json:"id"`
	Currency      string          `json:"currency"`
	Opportunities []v2ResponseOpp `json:"opportunities"`
	Error         *v2ErrorBody    `json:"error"`
}

type v2ResponseOpp struct {
	ID          string  `json:"id"`
	Bids        []v2Bid `json:"bids"`
	NoBidReason string  `json:"no_bid_reason"`
}

type v2Bid struct {
	ID         string       `json:"id"`
	PriceCPM   float64      `json:"price_cpm"`
	Currency   string       `json:"currency"`
	TTLMs      int64        `json:"ttl_ms"`
	Advertiser v2Advertiser `json:"advertiser"`
	Trackers   []v2Tracker  `json:"trackers"`
	Format     string       `json:"format"`
	Assets     v2BidAssets  `json:"assets"`
	Actions    v2Actions    `json:"actions"`
	// WinURL/LossURL are auction notices; Thrad asks for both because our
	// PBS auction is multi-bidder. WinURL rides on bid.burl (Prebid core
	// fires it on render). LossURL rides on bid.lurl and the native adm's
	// ext.thrad.lossUrl, which the client bundle fires with
	// ${AUCTION_LOSS} replaced when this bid loses the slot.
	WinURL  string `json:"win_url"`
	LossURL string `json:"loss_url"`
}

type v2Advertiser struct {
	Name    string `json:"name"`
	Domain  string `json:"domain"`
	LogoURL string `json:"logo_url"`
}

type v2Tracker struct {
	Event  string `json:"event"` // rendered | viewable | click
	Method string `json:"method"`
	URL    string `json:"url"`
}

type v2BidAssets struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	CTA         string   `json:"cta"`
	Image       *v2Image `json:"image"`
}

type v2Image struct {
	URL string `json:"url"`
}

type v2Actions struct {
	ClickURL    string `json:"click_url"`
	FeedbackURL string `json:"feedback_url"`
}

type v2ErrorBody struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Fields  json.RawMessage `json:"fields"`
}

// makeRequestV2 builds one v2 call for one imp. v2 only auctions the first
// opportunity in a request, so imps are never batched.
func (a *adapter) makeRequestV2(request *openrtb2.BidRequest, imp openrtb2.Imp, params openrtb_ext.ExtImpThrads) (*adapters.RequestData, error) {
	if imp.Native == nil {
		return nil, &errortypes.BadInput{
			Message: fmt.Sprintf("thrad: imp %s has no native object (only native is supported)", imp.ID),
		}
	}
	endpointStr, err := macros.ResolveMacros(a.v2Endpoint, macros.EndpointTemplateParams{})
	if err != nil {
		return nil, fmt.Errorf("thrad: failed to resolve endpoint: %w", err)
	}

	device := buildDevice(request.Device)
	context := buildContext(params)
	thradReq := v2Request{
		Version:       "2.0",
		ID:            request.ID,
		Context:       context,
		Opportunities: []v2Opportunity{buildOpportunity(imp, params, context != nil && context.Chat != nil)},
		User:          buildUser(params),
		Device:        device,
		Surface:       buildSurface(request, device),
		Privacy:       mergeUserMetaPrivacy(buildPrivacy(request), params.UserMeta),
	}

	body, err := json.Marshal(thradReq)
	if err != nil {
		return nil, fmt.Errorf("thrad: failed to marshal request for imp %s: %w", imp.ID, err)
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("Authorization", "Bearer "+a.selectKey(request, params.PublisherID))

	return &adapters.RequestData{
		Method:  http.MethodPost,
		Uri:     endpointStr,
		Body:    body,
		Headers: headers,
		ImpIDs:  []string{imp.ID},
	}, nil
}

// ── Request builders ─────────────────────────────────────────────────────────

const (
	maxIDLen      = 100
	maxSummaryLen = 10000
	minTitleChars = 30 // Thrad's floor for assets.title.max_chars
)

// validID enforces Thrad's 1–100 char, not-blank rule for context.id/user.id.
func validID(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= maxIDLen
}

// buildContext returns nil (pre-conversation, card only) when there's no
// usable conversation content — Thrad 400s a turn_number above 0 without a
// conversation, so turn_number is only ever sent alongside messages.
func buildContext(params openrtb_ext.ExtImpThrads) *v2Context {
	ctx := &v2Context{}
	if validID(params.ChatID) {
		ctx.ID = params.ChatID
	}
	if s := strings.TrimSpace(params.Summary); s != "" && len(params.Summary) <= maxSummaryLen {
		ctx.Summary = params.Summary
	}

	var msgs []v2Message
	userTurns := 0
	for _, m := range params.Messages {
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
			continue
		}
		msg := v2Message{Role: m.Role, Content: m.Content}
		if ts, err := time.Parse(time.RFC3339, m.Timestamp); err == nil {
			unix := ts.Unix()
			msg.Timestamp = &unix
		}
		if m.Role == "user" {
			userTurns++
		}
		msgs = append(msgs, msg)
	}
	// Thrad requires at least one user message for a chat context.
	if userTurns > 0 {
		turn := userTurns
		if params.TurnNumber != nil && *params.TurnNumber >= 0 {
			turn = *params.TurnNumber
		}
		ctx.Flow = "chat"
		ctx.Chat = &v2Chat{Messages: msgs, TurnNumber: &turn}
	}

	if ctx.ID == "" && ctx.Summary == "" && ctx.Chat == nil {
		return nil
	}
	return ctx
}

// nativeRequest is the subset of an OpenRTB Native 1.2 request we read
// asset length limits from.
type nativeRequest struct {
	Assets []struct {
		Title *struct {
			Len int `json:"len"`
		} `json:"title"`
		Data *struct {
			Type int `json:"type"`
			Len  int `json:"len"`
		} `json:"data"`
	} `json:"assets"`
}

// buildOpportunity requests a single "card" (v1's sponsored_message). Title
// and description length limits come from the Stored Imp's own native
// request, so Thrad's creative fits the slot it'll render into.
func buildOpportunity(imp openrtb2.Imp, params openrtb_ext.ExtImpThrads, contextual bool) v2Opportunity {
	assets := v2FormatAssets{
		Title:       &v2AssetSpec{},
		Description: &v2AssetSpec{},
		Image:       &v2AssetSpec{},
	}

	var native nativeRequest
	if json.Unmarshal([]byte(imp.Native.Request), &native) == nil {
		for _, as := range native.Assets {
			if as.Title != nil && as.Title.Len >= minTitleChars {
				n := as.Title.Len
				assets.Title.MaxChars = &n
			}
			if as.Data != nil && as.Data.Type == int(native1.DataAssetTypeDesc) && as.Data.Len > 0 {
				n := as.Data.Len
				assets.Description.MaxChars = &n
			}
		}
	}

	if cfg := params.Config; cfg != nil {
		if cfg.MaxHeadlineChars != nil && *cfg.MaxHeadlineChars >= minTitleChars {
			n := *cfg.MaxHeadlineChars
			assets.Title.MaxChars = &n
		}
		if cfg.ImageEnabled != nil && !*cfg.ImageEnabled {
			assets.Image = nil
		}
	}

	return v2Opportunity{
		ID:      opportunityID(imp, params, contextual),
		Formats: []v2Format{{Type: "card", Assets: assets}},
	}
}

// opportunityID is the stable id Thrad stores as the placement in its
// reporting: the Stored Imp id (copied into placementId by tpc.paramfanout),
// else the publisher, else the per-request imp id, suffixed with the kind of
// placement — ".opener" before the conversation, ".contextual" once a chat
// context is sent — so Thrad reports the two separately. The suffix is
// dropped rather than truncating an id past Thrad's length limit. Responses
// are matched to imps by call (one imp per call), not by this id — see
// makeBidsV2.
func opportunityID(imp openrtb2.Imp, params openrtb_ext.ExtImpThrads, contextual bool) string {
	id := imp.ID
	for _, candidate := range []string{params.PlacementID, params.PublisherID} {
		if validID(candidate) {
			id = candidate
			break
		}
	}
	kind := ".opener"
	if contextual {
		kind = ".contextual"
	}
	if validID(id + kind) {
		return id + kind
	}
	return id
}

var validAgeRanges = map[string]bool{
	"0-12": true, "13-17": true, "18-24": true, "25-34": true,
	"35-44": true, "45-54": true, "55-64": true, "65+": true,
}

func buildUser(params openrtb_ext.ExtImpThrads) *v2User {
	user := &v2User{}
	if validID(params.UserID) {
		user.ID = params.UserID
	}
	if params.UserMeta != nil && validAgeRanges[params.UserMeta.AgeRange] {
		user.AgeRange = params.UserMeta.AgeRange
	}
	// Thrad refuses anything but a lowercase 64-char hex hash, and a refused
	// field 400s the whole request, so a malformed value is dropped. Hex case
	// doesn't change the hash, so uppercase input is lowered rather than lost.
	if h := strings.ToLower(strings.TrimSpace(params.HashedEmail)); sha256Hex.MatchString(h) {
		user.ExtendedIDs = &v2ExtendedIDs{EmailsSHA256: []string{h}}
	}
	if user.ID == "" && user.AgeRange == "" && user.ExtendedIDs == nil {
		return nil
	}
	return user
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func buildDevice(d *openrtb2.Device) *v2Device {
	if d == nil {
		return nil
	}
	dev := &v2Device{
		IP:    publicIP(d.IP, d.IPv6),
		Model: d.Model,
	}
	if len(d.UA) <= 1000 {
		dev.UserAgent = d.UA
	}
	dev.OS = normalizeOS(d.OS, d.UA)
	dev.Type = deviceType(d.DeviceType, d.UA)
	if dev.OS != "" && d.OSV != "" && len(d.OSV) <= 64 {
		dev.OSVersion = d.OSV
	}
	if tag, err := language.Parse(d.Language); err == nil && d.Language != "" {
		dev.Language = tag.String()
	}

	if g := d.Geo; g != nil {
		geo := &v2Geo{
			Country: alpha2Country(g.Country),
			Region:  g.Region,
			City:    g.City,
			Zip:     g.ZIP,
		}
		if g.Lat != nil && g.Lon != nil {
			geo.Latitude, geo.Longitude = g.Lat, g.Lon
		}
		if *geo != (v2Geo{}) {
			dev.Geo = geo
		}
	}

	// idfa must pair with ios, gaid with android; the all-zero ID is refused.
	if d.IFA != "" && strings.Trim(d.IFA, "0-") != "" {
		switch dev.OS {
		case "ios":
			dev.IDs = &v2DeviceIDs{IDFA: d.IFA}
		case "android":
			dev.IDs = &v2DeviceIDs{GAID: d.IFA}
		}
	}

	if *dev == (v2Device{}) {
		return nil
	}
	return dev
}

// publicIP returns the end user's address only if Thrad will accept it: one
// bare, public IPv4/IPv6. Private, loopback, link-local, multicast and other
// reserved ranges are refused by Thrad with a 400, so they're dropped here.
func publicIP(ipv4, ipv6 string) string {
	for _, raw := range []string{ipv4, ipv6} {
		ip := net.ParseIP(raw)
		if ip == nil {
			continue
		}
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
			continue
		}
		return ip.String()
	}
	return ""
}

// alpha2Country converts OpenRTB's alpha-3 device.geo.country (or an alpha-2
// value some callers send) to the uppercase alpha-2 code Thrad requires.
// Anything that isn't a real country is dropped — Thrad falls back to its
// own IP lookup when geo.country is absent.
func alpha2Country(code string) string {
	if code == "" {
		return ""
	}
	r, err := language.ParseRegion(code)
	if err != nil || !r.IsCountry() {
		return ""
	}
	return r.String()
}

// normalizeOS maps OpenRTB's free-text device.os onto Thrad's enum, falling
// back to the user agent (browsers don't populate device.os).
func normalizeOS(os, ua string) string {
	switch o := strings.ToLower(strings.TrimSpace(os)); {
	case o == "ios" || o == "ipados" || o == "iphone os":
		return "ios"
	case o == "android":
		return "android"
	case strings.HasPrefix(o, "windows"):
		return "windows"
	case o == "macos" || o == "mac os" || o == "mac os x" || o == "os x" || o == "osx":
		return "macos"
	case o == "chromeos" || o == "chrome os" || o == "cros":
		return "chromeos"
	case o == "linux":
		return "linux"
	}
	// Order matters: Android UAs contain "Linux", iOS UAs contain "Mac OS X".
	switch {
	case ua == "":
		return ""
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad") || strings.Contains(ua, "iPod"):
		return "ios"
	case strings.Contains(ua, "Android"):
		return "android"
	case strings.Contains(ua, "CrOS"):
		return "chromeos"
	case strings.Contains(ua, "Windows"):
		return "windows"
	case strings.Contains(ua, "Macintosh") || strings.Contains(ua, "Mac OS X"):
		return "macos"
	case strings.Contains(ua, "Linux"):
		return "linux"
	}
	return ""
}

// deviceType maps OpenRTB devicetype (falling back to the UA) onto Thrad's
// mobile/tablet/desktop. CTV/STB/connected devices have no Thrad equivalent
// and are omitted.
func deviceType(dt adcom1.DeviceType, ua string) string {
	switch dt {
	case adcom1.DeviceMobile, adcom1.DevicePhone:
		return "mobile"
	case adcom1.DeviceTablet:
		return "tablet"
	case adcom1.DevicePC:
		return "desktop"
	case adcom1.DeviceTV, adcom1.DeviceConnected, adcom1.DeviceSetTopBox:
		return ""
	}
	switch {
	case ua == "":
		return ""
	case strings.Contains(ua, "iPad") || strings.Contains(ua, "Tablet") ||
		(strings.Contains(ua, "Android") && !strings.Contains(ua, "Mobile")):
		return "tablet"
	case strings.Contains(ua, "Mobi") || strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPod"):
		return "mobile"
	}
	return "desktop"
}

// storeForURL infers Thrad's surface.store from an app's store URL.
func storeForURL(storeURL string) string {
	u, err := url.Parse(storeURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.HasSuffix(host, "apple.com"):
		return "apple"
	case host == "play.google.com":
		return "google"
	case strings.Contains(host, "amazon."):
		return "amazon"
	case strings.HasSuffix(host, "samsung.com"):
		return "samsung"
	case strings.HasSuffix(host, "huawei.com"):
		return "huawei"
	case strings.HasSuffix(host, "microsoft.com"):
		return "microsoft"
	}
	return ""
}

// storeMatchesOS mirrors Thrad's rule that surface.store must fit device.os.
// An unknown OS is let through — Thrad only cross-checks when both are sent.
func storeMatchesOS(store, os string) bool {
	switch os {
	case "":
		return true
	case "ios", "macos":
		return store == "apple"
	case "android":
		return store == "google" || store == "amazon" || store == "samsung" || store == "huawei"
	case "windows":
		return store == "microsoft"
	}
	return false
}

// buildSurface describes where the ad renders. Thrad 400s a surface that's
// missing its type's required fields, so a partial app surface is omitted
// entirely rather than sent incomplete.
func buildSurface(request *openrtb2.BidRequest, device *v2Device) *v2Surface {
	if app := request.App; app != nil {
		store := storeForURL(app.StoreURL)
		os := ""
		if device != nil {
			os = device.OS
		}
		if store == "" || app.Bundle == "" || !storeMatchesOS(store, os) {
			return nil
		}
		return &v2Surface{Type: "app", Store: store, Bundle: app.Bundle, StoreURL: app.StoreURL}
	}
	if site := request.Site; site != nil {
		domain := site.Domain
		if domain == "" {
			if u, err := url.Parse(site.Page); err == nil {
				domain = u.Hostname()
			}
		}
		if domain == "" {
			return nil
		}
		return &v2Surface{Type: "web", Domain: strings.ToLower(domain)}
	}
	return nil
}

func buildPrivacy(request *openrtb2.BidRequest) *v2Privacy {
	p := &v2Privacy{}
	if regs := request.Regs; regs != nil {
		if regs.GDPR != nil {
			applies := *regs.GDPR == 1
			p.GDPRApplies = &applies
		}
		p.USPrivacy = regs.USPrivacy
		if p.USPrivacy == "" && len(regs.Ext) > 0 {
			var ext struct {
				USPrivacy string `json:"us_privacy"`
			}
			if json.Unmarshal(regs.Ext, &ext) == nil {
				p.USPrivacy = ext.USPrivacy
			}
		}
	}
	if user := request.User; user != nil {
		p.TCF = user.Consent
		if p.TCF == "" && len(user.Ext) > 0 {
			var ext struct {
				Consent string `json:"consent"`
			}
			if json.Unmarshal(user.Ext, &ext) == nil {
				p.TCF = ext.Consent
			}
		}
	}
	if d := request.Device; d != nil {
		isIOS := normalizeOS(d.OS, d.UA) == "ios"
		if isIOS && len(d.Ext) > 0 {
			// device.ext.atts: 0 not determined, 1 restricted, 2 denied, 3 authorized.
			var ext struct {
				ATTS *int `json:"atts"`
			}
			if json.Unmarshal(d.Ext, &ext) == nil && ext.ATTS != nil {
				p.ATTStatus = map[int]string{0: "not_determined", 1: "restricted", 2: "denied", 3: "authorized"}[*ext.ATTS]
			}
		}
		// limit_ad_tracking is non-iOS only; iOS uses att_status instead.
		if !isIOS && d.Lmt != nil {
			lat := *d.Lmt == 1
			p.LimitAdTracking = &lat
		}
	}
	if *p == (v2Privacy{}) {
		return nil
	}
	return p
}

// mergeUserMetaPrivacy lets publisher-supplied userMetadata consent strings
// (v1's user_metadata.tcf_consent / us_privacy) fill gaps in the OpenRTB
// ones. Returns a copy so the shared per-request privacy isn't mutated.
func mergeUserMetaPrivacy(p *v2Privacy, meta *openrtb_ext.ExtImpThradsUserMeta) *v2Privacy {
	if meta == nil || (meta.TCFConsent == "" && meta.USPrivacy == "") {
		return p
	}
	merged := v2Privacy{}
	if p != nil {
		merged = *p
	}
	if merged.TCF == "" {
		merged.TCF = meta.TCFConsent
	}
	if merged.USPrivacy == "" {
		merged.USPrivacy = meta.USPrivacy
	}
	return &merged
}

// ── Response handling ────────────────────────────────────────────────────────

// makeBidsV2 translates a v2 response into a native bid.
func (a *adapter) makeBidsV2(request *openrtb2.BidRequest, requestData *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, []error{statusError(response)}
	}

	var resp v2Response
	if err := json.Unmarshal(response.Body, &resp); err != nil {
		return nil, []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("thrad: failed to parse response: %s", err),
		}}
	}

	impIDs := map[string]bool{}
	for _, imp := range request.Imp {
		impIDs[imp.ID] = true
	}
	callImp := callImpID(request, requestData)

	var errs []error
	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(1)
	bidderResponse.Currency = "USD"

	for _, opp := range resp.Opportunities {
		if len(opp.Bids) == 0 {
			// A no-bid. Surfaced only on test requests, so a debugging session
			// sees Thrad's reason (no_fill, frequency_capped, unsafe_content…)
			// without adding a warning to every production auction.
			if request.Test == 1 && opp.NoBidReason != "" {
				errs = append(errs, &errortypes.Warning{
					Message: fmt.Sprintf("thrad: no bid for opportunity %s: %s", opp.ID, opp.NoBidReason),
				})
			}
			continue
		}

		// opportunities[].id is a stable placement id (see opportunityID),
		// not an imp id, so the call's own imp is the answer; an id that
		// does name an imp is honoured for safety.
		impID := callImp
		if impIDs[opp.ID] {
			impID = opp.ID
		}
		if impID == "" {
			errs = append(errs, &errortypes.BadServerResponse{
				Message: fmt.Sprintf("thrad: response opportunity %q matches no imp", opp.ID),
			})
			continue
		}

		// v2 only auctions the first opportunity, and we only ever send one.
		bid := opp.Bids[0]
		if bid.Format != "" && bid.Format != "card" {
			errs = append(errs, &errortypes.BadServerResponse{
				Message: fmt.Sprintf("thrad: unsupported bid format %q (only card is requested)", bid.Format),
			})
			continue
		}

		adm, trackerErrs := buildNativeAdm(bid)
		errs = append(errs, trackerErrs...)
		if adm == "" {
			continue
		}

		var adomain []string
		if bid.Advertiser.Domain != "" {
			adomain = []string{bid.Advertiser.Domain}
		}
		ortbBid := &openrtb2.Bid{
			ID:      bid.ID,
			ImpID:   impID,
			Price:   bid.PriceCPM,
			AdM:     adm,
			ADomain: adomain,
			CrID:    bid.ID,
			// Prebid core fires burl when the ad is rendered, which is when
			// Thrad asks for win_url. Nothing fires lurl server-side: the
			// client bundle fires the loss URL from the native adm's
			// ext.thrad.lossUrl (see buildNativeAdm) when this bid loses the
			// slot.
			BURL: bid.WinURL,
			LURL: bid.LossURL,
		}
		if bid.TTLMs > 0 {
			ortbBid.Exp = bid.TTLMs / 1000
		}
		if cur := firstNonEmpty(bid.Currency, resp.Currency); cur != "" {
			bidderResponse.Currency = cur
		}
		bidderResponse.Bids = append(bidderResponse.Bids, &adapters.TypedBid{
			Bid:     ortbBid,
			BidType: openrtb_ext.BidTypeNative,
		})
	}

	if len(bidderResponse.Bids) == 0 {
		return nil, errs
	}
	return bidderResponse, errs
}

// statusError maps a non-200 v2 response to a PBS error, branching on
// Thrad's error.code (their docs: branch on code, not message). 400s carry
// error.fields — the exact fields Thrad rejected — which are included so a
// debugging session can see them.
func statusError(response *adapters.ResponseData) error {
	var resp v2Response
	detail := string(response.Body)
	if json.Unmarshal(response.Body, &resp) == nil && resp.Error != nil {
		detail = resp.Error.Code + ": " + resp.Error.Message
		var fields bytes.Buffer
		if json.Compact(&fields, resp.Error.Fields) == nil && fields.Len() > 0 && fields.String() != "null" {
			detail += " fields=" + fields.String()
		}
	}
	msg := fmt.Sprintf("thrad: unexpected status code %d: %s", response.StatusCode, detail)
	if response.StatusCode == http.StatusBadRequest {
		return &errortypes.BadInput{Message: msg}
	}
	return &errortypes.BadServerResponse{Message: msg}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildNativeAdm builds an OpenRTB Native 1.2 adm from a v2 card bid.
//
// Asset IDs match v1 and the Stored Imps' native request (title→0, img→1,
// desc→2, sponsoredBy→3, cta→4) so Prebid.js's required-asset check passes.
//
// Trackers use native 1.2 eventtrackers, not imptrackers: rendered → event 1
// (fire on insert), viewable → event 2 (fire once MRC50-viewable), click →
// link.clicktrackers. imptrackers is deliberately left empty — the bundle's
// legacy path fires impressionTrackers at viewable time, which would report
// "rendered" late. The bundle's Imprezia-style eventtracker path is what
// fires these at the right moments.
func buildNativeAdm(bid v2Bid) (string, []error) {
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
		URL           string   `json:"url"`
		ClickTrackers []string `json:"clicktrackers,omitempty"`
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
	type nativeExtThrad struct {
		LossURL string `json:"lossUrl,omitempty"`
	}
	type nativeExt struct {
		Thrad *nativeExtThrad `json:"thrad,omitempty"`
	}
	type nativeAdm struct {
		Ver           string               `json:"ver"`
		Link          nativeLink           `json:"link"`
		Assets        []nativeAsset        `json:"assets"`
		EventTrackers []nativeEventTracker `json:"eventtrackers,omitempty"`
		Ext           *nativeExt           `json:"ext,omitempty"`
	}

	if bid.Assets.Title == "" || bid.Actions.ClickURL == "" {
		return "", []error{&errortypes.BadServerResponse{
			Message: fmt.Sprintf("thrad: bid %s is missing title or click_url", bid.ID),
		}}
	}

	assets := []nativeAsset{{ID: 0, Title: &nativeTitle{Text: bid.Assets.Title}}}
	if bid.Assets.Image != nil && bid.Assets.Image.URL != "" {
		assets = append(assets, nativeAsset{ID: 1, Img: &nativeImg{URL: bid.Assets.Image.URL, Type: int(native1.ImageAssetTypeMain)}})
	} else if bid.Advertiser.LogoURL != "" {
		assets = append(assets, nativeAsset{ID: 1, Img: &nativeImg{URL: bid.Advertiser.LogoURL, Type: int(native1.ImageAssetTypeIcon)}})
	}
	if bid.Assets.Description != "" {
		assets = append(assets, nativeAsset{ID: 2, Data: &nativeData{Value: bid.Assets.Description}})
	}
	if bid.Advertiser.Name != "" {
		assets = append(assets, nativeAsset{ID: 3, Data: &nativeData{Value: bid.Advertiser.Name}})
	}
	cta := bid.Assets.CTA
	if cta == "" {
		cta = "Learn More"
	}
	assets = append(assets, nativeAsset{ID: 4, Data: &nativeData{Value: cta}})

	adm := nativeAdm{
		Ver:    "1.2",
		Link:   nativeLink{URL: bid.Actions.ClickURL},
		Assets: assets,
	}
	// The browser decides which bid wins the slot, so it fires the loss
	// notice; this is where the bundle finds it (bid.native.ortb.ext.thrad).
	if bid.LossURL != "" {
		adm.Ext = &nativeExt{Thrad: &nativeExtThrad{LossURL: bid.LossURL}}
	}

	var errs []error
	for _, t := range bid.Trackers {
		if t.URL == "" {
			continue
		}
		if t.Method != "" && t.Method != "pixel" {
			errs = append(errs, &errortypes.BadServerResponse{
				Message: fmt.Sprintf("thrad: bid %s: unsupported tracker method %q for %s event, not fired", bid.ID, t.Method, t.Event),
			})
			continue
		}
		switch t.Event {
		case "rendered":
			adm.EventTrackers = append(adm.EventTrackers, nativeEventTracker{Event: int(native1.EventTypeImpression), Method: int(native1.EventTrackingMethodImage), URL: t.URL})
		case "viewable":
			adm.EventTrackers = append(adm.EventTrackers, nativeEventTracker{Event: int(native1.EventTypeViewableMRC50), Method: int(native1.EventTrackingMethodImage), URL: t.URL})
		case "click":
			adm.Link.ClickTrackers = append(adm.Link.ClickTrackers, t.URL)
		default:
			errs = append(errs, &errortypes.BadServerResponse{
				Message: fmt.Sprintf("thrad: bid %s: unknown tracker event %q, not fired", bid.ID, t.Event),
			})
		}
	}

	b, err := json.Marshal(adm)
	if err != nil {
		return "", append(errs, &errortypes.BadServerResponse{
			Message: fmt.Sprintf("thrad: failed to build native adm: %s", err),
		})
	}
	return string(b), errs
}
