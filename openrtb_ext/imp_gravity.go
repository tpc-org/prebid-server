package openrtb_ext

// ExtImpGravity defines the Gravity-specific imp params.
//
// Static fields (Placement, PlacementID) live in the PBS Stored Imp.
// Dynamic fields (UserID, SessionID, Messages) are injected per-auction
// by tpcBidAdapter from window.tpc.data and merged by PBS at request time.
type ExtImpGravity struct {
	// Dynamic fields — injected per-auction from window.tpc.data.
	UserID    string            `json:"userId"`
	SessionID string            `json:"sessionId"`
	Messages  []ExtImpGravityMsg `json:"messages,omitempty"`

	// Static fields — configured in the PBS Stored Imp.
	Placement   string `json:"placement"`   // e.g. "below_response"
	PlacementID string `json:"placementId"` // e.g. "chat-main"

	// Optional targeting.
	ExcludedTopics []string `json:"excludedTopics,omitempty"`
	Relevancy      *float64 `json:"relevancy,omitempty"`
	BidPrice       float64  `json:"bidPrice,omitempty"` // overrides global extra_info.bidPrice when set

	// Optional user identity signals.
	HashedEmail string `json:"hashedEmail,omitempty"`
	HashedPhone string `json:"hashedPhone,omitempty"`
}

// ExtImpGravityMsg is one turn of conversation history.
type ExtImpGravityMsg struct {
	Role    string `json:"role"`    // "user" or "assistant"
	Content string `json:"content"`
}
