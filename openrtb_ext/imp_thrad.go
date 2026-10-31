package openrtb_ext

// ExtImpThrads defines the Thrad-specific imp params.
//
// Static fields (RequestType, AdFormats) live in the PBS Stored Imp.
// Dynamic fields (UserID, ChatID, Messages, etc.) are injected per-auction
// by tpcBidAdapter from window.tpc.data and merged by PBS at request time.
type ExtImpThrads struct {
	// PublisherID selects which Thrad Publisher's API key pair to use, from
	// pbs.yaml adapters.thrad.extra_info's "publishers" map (see thrad.go's
	// package doc). Set this to our own Publisher.slug (sayhola, drawify,
	// learnrithm, artsmart, slashspace). When empty, or when no entry exists
	// for it, the adapter falls back to extra_info's top-level default key pair.
	PublisherID string `json:"publisherId,omitempty"`

	// Protocol selects Thrad's bid request protocol for this placement: 2
	// (v2) or 1/unset (v1, the default until the cutover; the "thradx" alias
	// defaults to 2). Static, set in the Stored Imp — lets placements move
	// to v2 one at a time. Remove once v1 is gone.
	Protocol int `json:"protocol,omitempty"`

	// PlacementID is a stable placement identifier sent to Thrad v2 as
	// opportunities[].id, which Thrad stores as the placement in its
	// reporting. Filled automatically from the Stored Imp id by the
	// tpc.paramfanout module; a Stored Imp may also set it explicitly.
	PlacementID string `json:"placementId,omitempty"`

	// RequestType is "contextual" (default) or "opener".
	RequestType string `json:"requestType,omitempty"`

	// AdFormats lists accepted ad format types. Defaults to ["sponsored_message"].
	AdFormats []string `json:"adFormats,omitempty"`

	// Dynamic fields — injected per-auction from window.tpc.data via tpcBidAdapter.
	UserID     string            `json:"userId"`
	ChatID     string            `json:"chatId,omitempty"`
	Messages   []ExtImpThradsMsg `json:"messages,omitempty"`
	Summary    string            `json:"summary,omitempty"`
	TurnNumber *int              `json:"turnNumber,omitempty"`

	// HashedEmail is a logged-in user's SHA-256(trim(lowercase(email))) hex,
	// hashed publisher-side (window.tpc.data.hashedEmail). Sent to v2 only,
	// as user.extended_ids.emails_sha256; v1 has no field for it. Never
	// required — anonymous users have none.
	HashedEmail string `json:"hashedEmail,omitempty"`

	// Optional publisher-side config and user metadata.
	Config   *ExtImpThradsConfig   `json:"config,omitempty"`
	UserMeta *ExtImpThradsUserMeta `json:"userMetadata,omitempty"`
}

// ExtImpThradsMsg is one turn of conversation history.
type ExtImpThradsMsg struct {
	Role      string `json:"role"` // "user" or "assistant"
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"` // ISO 8601
}

// ExtImpThradsConfig carries optional publisher display config.
type ExtImpThradsConfig struct {
	AdOffset            *int   `json:"ad_offset,omitempty"`
	MaxFrequency        *int   `json:"max_frequency,omitempty"`
	MaxHeadlineChars    *int   `json:"max_headline_chars,omitempty"`
	ImageEnabled        *bool  `json:"image_enabled,omitempty"`
	RestrictedClickArea *bool  `json:"restricted_click_area,omitempty"`
	ExperimentTag       string `json:"experiment_tag,omitempty"`
}

// ExtImpThradsUserMeta carries optional targeting and compliance metadata.
type ExtImpThradsUserMeta struct {
	AgeRange   string `json:"age_range,omitempty"`
	TCFConsent string `json:"tcf_consent,omitempty"`
	USPrivacy  string `json:"us_privacy,omitempty"`
}
