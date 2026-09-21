package openrtb_ext

// ExtImpImprezia defines the Imprezia-specific imp params.
//
// Dynamic fields (Request, Response, Timestamp, DeviceContext) are
// injected per-auction — Request is the user's query, Response is the
// AI-generated text to monetize, Timestamp is when that reply completed,
// DeviceContext is the end user's viewport/device shape. These are the
// four genuinely per-auction fields among the set Imprezia's real API
// (POST /v1/ads/chat) requires — confirmed against their live Chat Ads
// API REST reference (https://demo.imprezia.ai/docs, "Chat Ads API"
// platform tab — distinct from the Web SDK tab this adapter was
// originally built against, which is what it renders by default).
// Imprezia also requires SourceURL and PlatformString, and two headers
// (X-Forwarded-User-Agent, X-Forwarded-For) — those four are NOT dynamic
// client fields, so they have no entry here; imprezia.go's MakeRequests
// derives them itself from the OpenRTB request (request.Site.Page,
// request.Device.UA/IP) and a hardcoded PlatformString.
//
// None of these fields — including Request/Response — are marked
// "required" in bidder-params/imprezia.json's JSON schema, and neither
// should they be. Real production incident, 2026-08-22 (learnrithm): an
// earlier version of this schema required Request/Response reasoning that
// Imprezia's own API needs them — but that's exactly the field pair that
// goes missing on any auction fired before an assistant reply exists
// (page load, or the instant a prompt is submitted). PBS's static schema
// validation runs before the adapter ever sees the imp, so a "required"
// field with no value present rejects the WHOLE imp — not just Imprezia's
// bid, killing Thrad too on that same request. See imprezia.go's package
// doc for the full incident writeup, and its 2026-08-23 addendum for why
// Timestamp/DeviceContext follow the identical rule. Do NOT add Request,
// Response, Timestamp, DeviceContext (or anything else) to that schema's
// "required" list — MakeRequests already checks for them itself and skips
// the imp gracefully (soft per-bidder error, no bid) when any is missing,
// same contract Gravity already has for a missing `messages`.
type ExtImpImprezia struct {
	// Dynamic — injected per-auction from window.tpc.data via the client
	// bundle (prebid-deployments), same convention as Thrad/Gravity's
	// dynamic fields. Required by Imprezia's own API; MakeRequests skips
	// (does not send) an imp missing any of these rather than sending a
	// request Imprezia will reject.
	Request   string `json:"request"`
	Response  string `json:"response"`
	Timestamp string `json:"timestamp"`

	// DeviceContext is the viewport the ad would render into. The client
	// bundle sources ViewportWidth/ViewportHeight from window.innerWidth/
	// innerHeight rather than a specific ad container's rendered box —
	// see prebid-deployments' buildImpreziaParams() doc comment for why
	// (the ad unit's params are shared across all slot instances in a
	// grid, so there's no single "this slot" element at request-build
	// time).
	DeviceContext *ExtImpImpreziaDeviceContext `json:"deviceContext,omitempty"`

	// Optional tracking/personalization identifiers.
	UserID    string `json:"userId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`

	// Static — set in the PBS Stored Imp. SiteID is Imprezia's UUID
	// "surface" ID for this publisher (sayhola's is
	// cbc68717-3d85-4d55-9a29-e7a1a22ab4ef); PlacementID is our own
	// human-readable label (e.g. "chat_followup") — per Imprezia's
	// attribution rules, an unregistered PlacementID auto-registers under
	// SiteID on first successful request, no separate registration call.
	SiteID      string `json:"siteId,omitempty"`
	PlacementID string `json:"placementId,omitempty"`

	// MaxCards caps the number of sponsored cards Imprezia returns
	// (their own default is 2). We request 1 by default (one PBS bid per
	// imp) — see imprezia.go.
	MaxCards *int `json:"maxCards,omitempty"`

	// BidPrice overrides the global extra_info.bidPrice fallback. Imprezia
	// started returning its own ad.bidPrice/bidCurrency 2026-09-21, which
	// now takes precedence over this when present — see imprezia.go's
	// package doc "Price field" section. This field (and the extra_info
	// default) remain only as a defensive fallback for a response that
	// omits it.
	BidPrice float64 `json:"bidPrice,omitempty"`
}

// ExtImpImpreziaDeviceContext is Imprezia's required `deviceContext`
// object: DeviceType is "desktop" | "mobile" | "tablet" (a simple width
// breakpoint client-side, not real device detection); ViewportWidth/
// ViewportHeight are CSS pixels.
type ExtImpImpreziaDeviceContext struct {
	DeviceType     string `json:"deviceType"`
	ViewportWidth  int    `json:"viewportWidth"`
	ViewportHeight int    `json:"viewportHeight"`
}
