package openrtb_ext

// ExtImpThrads defines the Thrad-specific imp params.
//
// Static fields (RequestType, AdFormats) live in the PBS Stored Imp.
// Dynamic fields (UserID, ChatID, Messages, etc.) are injected per-auction
// by tpcBidAdapter from window.tpc.data and merged by PBS at request time.
type ExtImpThrads struct {
	// PublisherID is accepted for compatibility but ignored — API keys come from pbs.yaml extra_info.
	PublisherID string `json:"publisherId,omitempty"`

	// RequestType is "contextual" (default) or "opener".
	RequestType string `json:"requestType,omitempty"`

	// AdFormats lists accepted ad format types. Defaults to ["sponsored_message"].
	AdFormats []string `json:"adFormats,omitempty"`

	// Dynamic fields — injected per-auction from window.tpc.data via tpcBidAdapter.
	UserID     string              `json:"userId"`
	ChatID     string              `json:"chatId,omitempty"`
	Messages   []ExtImpThradsMsg   `json:"messages,omitempty"`
	Summary    string              `json:"summary,omitempty"`
	TurnNumber *int                `json:"turnNumber,omitempty"`

	// Optional publisher-side config and user metadata.
	Config   *ExtImpThradsConfig   `json:"config,omitempty"`
	UserMeta *ExtImpThradsUserMeta `json:"userMetadata,omitempty"`
}

// ExtImpThradsMsg is one turn of conversation history.
type ExtImpThradsMsg struct {
	Role      string `json:"role"`      // "user" or "assistant"
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
