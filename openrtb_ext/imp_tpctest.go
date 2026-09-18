package openrtb_ext

// ExtImpTpcTest defines the tpctest bidder's imp params.
//
// tpctest is a deterministic, SYNTHETIC bidder used only to validate a
// server-side integration partner's end-to-end pipeline — never a real
// demand source. See docs/integration/test-bidder-plan.md (tpc-org/docs)
// and adapters/tpctest/tpctest.go's package doc.
//
// BidPrice is the only field, and it's optional — nothing here is
// JSON-schema "required" in static/bidder-params/tpctest.json, matching
// the AdSense/Imprezia convention.
type ExtImpTpcTest struct {
	// BidPrice overrides the adapter's own default fixed CPM. tpctest
	// never makes a real per-auction call, so this (or the default) is
	// the only price signal that exists.
	BidPrice float64 `json:"bidPrice,omitempty"`
}
