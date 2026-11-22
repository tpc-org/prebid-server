package adgentek

import (
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/adapters/adapterstest"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testEndpoint = "http://any.url/ad-server/v1/get-ad"

var testServer = config.Server{ExternalUrl: "http://hosturl.com", GvlID: 1, DataCenter: "2"}

func TestJsonSamples(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderAdgentek, config.Adapter{
		Endpoint:         testEndpoint,
		ExtraAdapterInfo: `{"apiKey":"test-key"}`,
	}, testServer)
	require.NoError(t, err)
	adapterstest.RunJSONBidderTest(t, "adgentektest", bidder)
}

func TestEmptyConfig(t *testing.T) {
	_, err := Builder(openrtb_ext.BidderAdgentek, config.Adapter{Endpoint: testEndpoint}, testServer)
	assert.EqualError(t, err, "adgentek: extra_info with apiKey is required")
}

func TestBadConfig(t *testing.T) {
	for name, extraInfo := range map[string]string{
		"not json":    `{foo:42}`,
		"key missing": `{}`,
		"key blank":   `{"apiKey":"  "}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Builder(openrtb_ext.BidderAdgentek, config.Adapter{Endpoint: testEndpoint, ExtraAdapterInfo: extraInfo}, testServer)
			assert.Error(t, err)
		})
	}
}

func TestEndpointMissing(t *testing.T) {
	_, err := Builder(openrtb_ext.BidderAdgentek, config.Adapter{ExtraAdapterInfo: `{"apiKey":"test-key"}`}, testServer)
	assert.EqualError(t, err, "adgentek: endpoint is not configured")
}

// With several imps, each response is mapped to its call's imp by ImpIDs
// (the JSON harness never passes ImpIDs to MakeBids, so this is tested here).
func TestMakeBidsMapsByImpIDs(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderAdgentek, config.Adapter{Endpoint: testEndpoint, ExtraAdapterInfo: `{"apiKey":"k"}`}, testServer)
	require.NoError(t, err)
	native := `{"ver":"1.2","assets":[{"id":7,"required":1,"title":{"len":80}}]}`
	request := &openrtb2.BidRequest{Imp: []openrtb2.Imp{
		{ID: "imp-1", Native: &openrtb2.Native{Request: native}},
		{ID: "imp-2", Native: &openrtb2.Native{Request: native}},
	}}
	body := `{"ads":[{"id":"ad-1","title":"T","click_url":"https://c","impression_url":"https://i","bid_price":1.5}]}`
	resp := &adapters.ResponseData{StatusCode: 200, Body: []byte(body)}

	out, errs := bidder.MakeBids(request, &adapters.RequestData{ImpIDs: []string{"imp-2"}}, resp)
	require.Empty(t, errs)
	require.Len(t, out.Bids, 1)
	assert.Equal(t, "imp-2", out.Bids[0].Bid.ImpID)
	assert.Equal(t, `{"ver":"1.2","assets":[{"id":7,"title":{"text":"T"}}],"link":{"url":"https://c"},"eventtrackers":[{"event":1,"method":1,"url":"https://i"}]}`, out.Bids[0].Bid.AdM)

	_, errs = bidder.MakeBids(request, &adapters.RequestData{}, resp)
	assert.Len(t, errs, 1)
}

func TestAlpha2Country(t *testing.T) {
	for in, want := range map[string]string{
		"USA": "US", "GBR": "GB", "deu": "DE", "US": "US", "": "", "EU": "", "XXX": "",
	} {
		assert.Equal(t, want, alpha2Country(in), in)
	}
}

func TestTruncateCountsRunes(t *testing.T) {
	assert.Equal(t, "héllo", truncate("héllo wörld", 5))
	assert.Equal(t, "héllo wörld", truncate("héllo wörld", 0))
	assert.Equal(t, "ab", truncate("ab", 5))
}
