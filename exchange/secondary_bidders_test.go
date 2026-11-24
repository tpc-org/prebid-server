package exchange

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/currency"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/hooks/hookexecution"
	metricsConfig "github.com/prebid/prebid-server/v4/metrics/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
)

const slowSecondaryDelay = 2 * time.Second

func secondaryTestBidder(t *testing.T, name openrtb_ext.BidderName, delay time.Duration, bidID string) AdaptedBidder {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return AdaptBidder(&goodSingleBidder{
		httpRequest: &adapters.RequestData{Method: "POST", Uri: server.URL},
		bidResponse: &adapters.BidderResponse{Bids: []*adapters.TypedBid{{Bid: &openrtb2.Bid{ID: bidID, ImpID: "imp-1"}}}},
	}, server.Client(), &config.Configuration{}, &metricsConfig.NilMetricsEngine{}, name, nil, "")
}

// noRequestBidder makes no outbound request, like TPC's "tpc" stub adapter.
func noRequestBidder(name openrtb_ext.BidderName) AdaptedBidder {
	return AdaptBidder(&goodSingleBidderWithStoredBidResp{}, http.DefaultClient, &config.Configuration{},
		&metricsConfig.NilMetricsEngine{}, name, nil, "")
}

func secondaryTestRequests(names ...openrtb_ext.BidderName) []BidderRequest {
	out := make([]BidderRequest, 0, len(names))
	for _, n := range names {
		out = append(out, BidderRequest{
			BidderName:     n,
			BidderCoreName: n,
			BidRequest:     &openrtb2.BidRequest{ID: "req", Imp: []openrtb2.Imp{{ID: "imp-1"}}},
		})
	}
	return out
}

func runSecondaryAuction(adapterMap map[openrtb_ext.BidderName]AdaptedBidder, requests []BidderRequest,
	secondary map[openrtb_ext.BidderName]struct{}) (map[openrtb_ext.BidderName]bool, map[openrtb_ext.BidderName]*seatResponseExtra, extraAuctionResponseInfo, time.Duration) {
	e := exchange{me: &metricsConfig.NilMetricsEngine{}, adapterMap: adapterMap}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bids, extra, info := e.getAllBids(ctx, requests, nil, &currency.ConstantRates{}, false, "", false,
		openrtb_ext.ExtAlternateBidderCodes{}, nil, hookexecution.EmptyHookExecutor{}, time.Now(), nil, nil, false, nil, secondary)
	got := map[openrtb_ext.BidderName]bool{}
	for name, seat := range bids {
		got[name] = len(seat.Bids) > 0
	}
	return got, extra, info, time.Since(start)
}

func TestSecondaryBidderCutOffAfterPrimaries(t *testing.T) {
	primary, secondary := openrtb_ext.BidderName("pubmatic"), openrtb_ext.BidderName("appnexus")
	adapterMap := map[openrtb_ext.BidderName]AdaptedBidder{
		primary:   secondaryTestBidder(t, primary, 0, "p"),
		secondary: secondaryTestBidder(t, secondary, slowSecondaryDelay, "s"),
	}
	bids, extra, info, elapsed := runSecondaryAuction(adapterMap, secondaryTestRequests(primary, secondary),
		resolveSecondaryBidders([]string{"appnexus"}, nil))

	assert.Less(t, elapsed, slowSecondaryDelay, "auction should not wait for the slow secondary bidder")
	assert.True(t, bids[primary])
	assert.False(t, bids[secondary])
	assert.Equal(t, []openrtb_ext.ExtBidderMessage{{Code: errortypes.TimeoutErrorCode, Message: secondaryBidderTimeoutMessage}},
		extra[secondary].Warnings)
	nonBids := info.seatNonBidBuilder.Slice()
	if assert.Len(t, nonBids, 1) {
		assert.Equal(t, "appnexus", nonBids[0].Seat)
		assert.Equal(t, int(ErrorTimeout), nonBids[0].NonBid[0].StatusCode)
		assert.Equal(t, "imp-1", nonBids[0].NonBid[0].ImpId)
	}
}

func TestSecondaryBidderFastEnoughCounts(t *testing.T) {
	primary, secondary := openrtb_ext.BidderName("pubmatic"), openrtb_ext.BidderName("appnexus")
	adapterMap := map[openrtb_ext.BidderName]AdaptedBidder{
		primary:   secondaryTestBidder(t, primary, 300*time.Millisecond, "p"),
		secondary: secondaryTestBidder(t, secondary, 0, "s"),
	}
	bids, extra, _, _ := runSecondaryAuction(adapterMap, secondaryTestRequests(primary, secondary),
		resolveSecondaryBidders([]string{"appnexus"}, nil))

	assert.True(t, bids[primary])
	assert.True(t, bids[secondary], "a secondary bid that arrives before the primaries finish is kept")
	assert.Empty(t, extra[secondary].Warnings)
}

func TestSecondaryBidderNoRequestPrimaryDoesNotEndAuction(t *testing.T) {
	stub, secondary := openrtb_ext.BidderName("pubmatic"), openrtb_ext.BidderName("appnexus")
	adapterMap := map[openrtb_ext.BidderName]AdaptedBidder{
		stub:      noRequestBidder(stub),
		secondary: secondaryTestBidder(t, secondary, 200*time.Millisecond, "s"),
	}
	bids, _, _, _ := runSecondaryAuction(adapterMap, secondaryTestRequests(stub, secondary),
		resolveSecondaryBidders([]string{"appnexus"}, nil))

	assert.True(t, bids[secondary], "a no-request primary must not cut the secondary off")
}

func TestSecondaryBiddersAllSecondaryWaitsForEveryone(t *testing.T) {
	a, b := openrtb_ext.BidderName("pubmatic"), openrtb_ext.BidderName("appnexus")
	adapterMap := map[openrtb_ext.BidderName]AdaptedBidder{
		a: secondaryTestBidder(t, a, 0, "a"),
		b: secondaryTestBidder(t, b, 200*time.Millisecond, "b"),
	}
	bids, _, _, _ := runSecondaryAuction(adapterMap, secondaryTestRequests(a, b),
		resolveSecondaryBidders([]string{"pubmatic", "appnexus"}, nil))

	assert.True(t, bids[a])
	assert.True(t, bids[b])
}

func TestSecondaryBiddersNoneConfiguredWaitsForEveryone(t *testing.T) {
	a, b := openrtb_ext.BidderName("pubmatic"), openrtb_ext.BidderName("appnexus")
	adapterMap := map[openrtb_ext.BidderName]AdaptedBidder{
		a: secondaryTestBidder(t, a, 0, "a"),
		b: secondaryTestBidder(t, b, 200*time.Millisecond, "b"),
	}
	bids, _, _, _ := runSecondaryAuction(adapterMap, secondaryTestRequests(a, b), nil)

	assert.True(t, bids[a])
	assert.True(t, bids[b])
}

func TestResolveSecondaryBidders(t *testing.T) {
	set := func(names ...string) map[openrtb_ext.BidderName]struct{} {
		m := map[openrtb_ext.BidderName]struct{}{}
		for _, n := range names {
			m[openrtb_ext.BidderName(n)] = struct{}{}
		}
		return m
	}
	assert.Nil(t, resolveSecondaryBidders(nil, nil))
	assert.Equal(t, set("kontext"), resolveSecondaryBidders(nil, []string{"kontext"}), "account list applies")
	assert.Equal(t, set("adgentek"), resolveSecondaryBidders([]string{"adgentek"}, []string{"kontext"}), "request replaces account")
	assert.Nil(t, resolveSecondaryBidders([]string{}, []string{"kontext"}), "an empty request list clears the account's")
}
