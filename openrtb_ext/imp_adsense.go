package openrtb_ext

// ExtImpAdsense defines the AdSense-specific imp params.
//
// Unlike every other bidder in this fork, AdSense has no dynamic
// per-auction fields at all — the client/slot ID are constants baked
// into adapters/adsense/adsense.go (the same AdSense ad unit is reused
// for every publisher and placement), and this adapter makes no real
// call to Google, so there's nothing per-auction for the client bundle
// to inject. See docs/integration/adsense-integration-plan.md.
//
// BidPrice is the only field, and it's optional — nothing here is
// JSON-schema "required" in static/bidder-params/adsense.json, matching
// the Gravity/Imprezia convention even though (unlike those two) there's
// no dynamic field to protect from PBS's whole-imp-rejection behavior;
// it's just consistent practice.
type ExtImpAdsense struct {
	// BidPrice overrides the global extra_info.bidPrice fallback CPM.
	// AdSense has no price in its own response — there is no response,
	// by design (see the adsense package doc) — so this (or the global
	// fallback) is the only price signal that exists.
	BidPrice float64 `json:"bidPrice,omitempty"`
}
