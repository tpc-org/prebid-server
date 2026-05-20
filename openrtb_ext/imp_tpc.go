package openrtb_ext

// ExtImpTpc defines the contract for imp.ext.bidder when bidder = "tpc".
//
// These params are sent by the Prebid.js tpcBidAdapter and validated against
// static/bidder-params/tpc.json before reaching the adapter.
//
// accountId: publisher account ID on pbs.tpcsrv.com. Required for param
// validation; reserved for future server-side account routing.
//
// placementId: the stored imp ID. The Prebid.js adapter writes this into
// imp[].ext.prebid.storedrequest.id, which PBS core uses to load the stored
// imp before this adapter is invoked. The adapter itself does not use this
// field — it is present here for completeness and forward compatibility.
type ExtImpTpc struct {
	AccountID   string `json:"accountId"`
	PlacementID string `json:"placementId,omitempty"`
}
