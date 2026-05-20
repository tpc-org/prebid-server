package tpc

// Package tpc implements the PBS bidder adapter for the "tpc" bidder code.
//
// This adapter is a routing stub. Its sole purpose is to register the "tpc"
// bidder name with Prebid Server so that auction requests from the TPC
// Prebid.js adapter pass PBS bidder validation.
//
// The Prebid.js tpcBidAdapter sends imp.ext.prebid.bidder.tpc on every
// request. PBS validates this against its registered bidder list and rejects
// unknown bidders. This adapter satisfies that validation requirement.
//
// Actual demand is resolved entirely server-side via PBS Stored Imps:
//
//	imp[].ext.prebid.storedrequest.id  →  stored_imps/<client>/<id>.json
//
// PBS merges the stored imp into the request before any adapter is called.
// The stored imp carries the real bidder config (e.g. adform params).
// This adapter never makes an outbound HTTP request.
//
// Adding or changing bidders requires only a pbs-settings stored imp change —
// no change to this adapter and no redeploy of the binary is needed.
//
// Fork-only per FORK_NOTES.md — do not include in upstream PBS PRs.

import (
	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// adapter implements the adapters.Bidder interface as a no-op routing stub.
type adapter struct{}

// Builder satisfies the adapter builder signature required by exchange/adapter_util.go.
func Builder(_ openrtb_ext.BidderName, _ config.Adapter, _ config.Server) (adapters.Bidder, error) {
	return &adapter{}, nil
}

// MakeRequests returns nil. This adapter never makes outbound HTTP requests.
// All demand is sourced from PBS stored imps — no SSP endpoint is called.
func (a *adapter) MakeRequests(
	_ *openrtb2.BidRequest,
	_ *adapters.ExtraRequestInfo,
) ([]*adapters.RequestData, []error) {
	return nil, nil
}

// MakeBids returns nil. MakeRequests never produces requests, so there are
// no responses to parse.
func (a *adapter) MakeBids(
	_ *openrtb2.BidRequest,
	_ *adapters.RequestData,
	_ *adapters.ResponseData,
) (*adapters.BidderResponse, []error) {
	return nil, nil
}
