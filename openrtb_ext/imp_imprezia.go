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
// UserID/SessionID/SiteID/PlacementID are genuinely optional per Imprezia's
// live docs (unlike Gravity, where userId/sessionId are required). Do NOT
// add these to bidder-params/imprezia.json's "required" list — see that
// file's own comment for why (this repeats an incident class already hit
// once with Gravity: PBS rejects the WHOLE imp, not just one bidder, when
// a multi-bidder stored imp's declared-required field is missing).
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
