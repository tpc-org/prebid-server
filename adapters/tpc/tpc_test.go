package tpc

import (
	"testing"

	"github.com/prebid/prebid-server/v4/adapters/adapterstest"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
)

// TestBuilder confirms the adapter constructs without error.
func TestBuilder(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderName("tpc"), config.Adapter{}, config.Server{})
	assert.NoError(t, err)
	assert.NotNil(t, bidder)
}

// TestMakeRequestsReturnsNil confirms no outbound HTTP requests are made.
// All demand is resolved via PBS stored imps; this adapter is a routing stub.
func TestMakeRequestsReturnsNil(t *testing.T) {
	bidder, _ := Builder(openrtb_ext.BidderName("tpc"), config.Adapter{}, config.Server{})
	reqs, errs := bidder.MakeRequests(nil, nil)
	assert.Nil(t, reqs)
	assert.Nil(t, errs)
}

// TestMakeBidsReturnsNil confirms no bids are produced directly by this adapter.
func TestMakeBidsReturnsNil(t *testing.T) {
	bidder, _ := Builder(openrtb_ext.BidderName("tpc"), config.Adapter{}, config.Server{})
	resp, errs := bidder.MakeBids(nil, nil, nil)
	assert.Nil(t, resp)
	assert.Nil(t, errs)
}

// TestJSONBidderTest runs the golden-file test suite from tpctest/.
func TestJSONBidderTest(t *testing.T) {
	adapterstest.RunJSONBidderTest(t, "tpc", new(adapter))
}
