package openrtb_ext

// ExtImpImprezia defines the Imprezia-specific imp params.
//
// Dynamic fields (Request, Response) are injected per-auction — Request is
// the user's query, Response is the AI-generated text to monetize. These
// are the ONLY two fields Imprezia's real API (POST /v1/ads/chat) requires
// — confirmed via their docs' TypeScript SDK types (MonetizeOptions),
// which their docs state map 1:1 onto the raw REST contract, plus live
// testing of the endpoint itself.
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
// doc for the full incident writeup. Do NOT add Request or Response (or
// anything else) to that schema's "required" list — MakeRequests already
// checks for them itself and skips the imp gracefully (soft per-bidder
// error, no bid) when either is empty, same contract Gravity already has
// for a missing `messages`.
type ExtImpImprezia struct {
	// Dynamic — injected per-auction from window.tpc.data via the client
	// bundle (prebid-deployments), same convention as Thrad/Gravity's
	// dynamic fields. Required by Imprezia's own API; MakeRequests skips
	// (does not send) an imp missing either of these rather than sending a
	// request Imprezia will reject.
	Request  string `json:"request"`
	Response string `json:"response"`

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

	// BidPrice overrides the global extra_info.bidPrice fallback — Imprezia
	// returns no price field in its response, same gap as Gravity.
	BidPrice float64 `json:"bidPrice,omitempty"`
}
