package openrtb_ext

// ExtImpKontext defines the Kontext imp params. Both are static and live in
// the Stored Imp. Neither is required by the schema: PBS validates params
// before the adapter runs, and a required field that is absent rejects the
// whole imp, other bidders included.
type ExtImpKontext struct {
	// SellerID is the publisher's seller_id in sayhola.ai/sellers.json. It
	// becomes the sid of our supply-chain node.
	SellerID string `json:"sellerId,omitempty"`
	// PlacementID is sent as imp.tagid. The adapter cannot see the Stored Imp
	// id itself, so the Stored Imp repeats it here.
	PlacementID string `json:"placementId,omitempty"`
}
