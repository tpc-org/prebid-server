package openrtb_ext

// ExtImpGravity defines the Gravity-specific imp params.
//
// Static fields (Placement, PlacementID) live in the PBS Stored Imp.
// Dynamic fields (UserID, SessionID, Messages) are injected per-auction
// by tpcBidAdapter from window.tpc.data and merged by PBS at request time.
type ExtImpGravity struct {
	// Dynamic fields — injected per-auction from window.tpc.data.
	UserID    string             `json:"userId"`
	SessionID string             `json:"sessionId"`
	Messages  []ExtImpGravityMsg `json:"messages,omitempty"`

	// Static fields — configured in the PBS Stored Imp.
	Placement   string `json:"placement"`   // e.g. "below_response"
	PlacementID string `json:"placementId"` // e.g. "chat-main"

	// Optional targeting.
	ExcludedTopics []string `json:"excludedTopics,omitempty"`
	Relevancy      *float64 `json:"relevancy,omitempty"`
	BidPrice       float64  `json:"bidPrice,omitempty"` // overrides global extra_info.bidPrice when set

	// Optional user identity signals for logged-in users — added 2026-08-10.
	// Hashing happens publisher-side (via window.tpc.data.hashedEmail — see
	// docs/integration/external-integration.md), never in our own client
	// bundle or PBS: we never see the plain email at all. Gravity requires
	// SHA-256 of email.strip().lower() specifically (their normalization
	// requirement, confirmed against their OpenAPI spec) — a different hash
	// algorithm or unnormalized input won't match their identity graph, but
	// PBS has no way to detect that; it's the publisher's responsibility to
	// hash correctly before ever setting this field.
	//
	// Thrad has no equivalent field and no hashed-email support at all —
	// their docs explicitly say not to put email/name/PII in their userId
	// field, so this is Gravity-only, not a general "user identity" concept
	// shared across both bidders. See adapters/gravity/gravity.go's
	// gravityUser.EmailHash for the (differently-named) field this maps to
	// in Gravity's actual API request.
	HashedEmail string `json:"hashedEmail,omitempty"`
	HashedPhone string `json:"hashedPhone,omitempty"`
}

// ExtImpGravityMsg is one turn of conversation history.
type ExtImpGravityMsg struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}
