package kontext

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

const testEndpoint = "http://any.url"

var testServer = config.Server{ExternalUrl: "http://hosturl.com", GvlID: 1, DataCenter: "2"}

func TestJsonSamples(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderKontext, config.Adapter{
		Endpoint:         testEndpoint,
		ExtraAdapterInfo: `{"partnerId":"hola-test","token":"test-token"}`,
	}, testServer)
	require.NoError(t, err)
	adapterstest.RunJSONBidderTest(t, "kontexttest", bidder)
}

func TestEmptyConfig(t *testing.T) {
	_, err := Builder(openrtb_ext.BidderKontext, config.Adapter{Endpoint: testEndpoint}, testServer)
	assert.EqualError(t, err, "kontext: extra_info with partnerId and token is required")
}

func TestBadConfig(t *testing.T) {
	for name, extraInfo := range map[string]string{
		"not json":           `{foo:42}`,
		"token missing":      `{"partnerId":"hola-test"}`,
		"partner id missing": `{"token":"test-token"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Builder(openrtb_ext.BidderKontext, config.Adapter{Endpoint: testEndpoint, ExtraAdapterInfo: extraInfo}, testServer)
			assert.Error(t, err)
		})
	}
}

func TestEndpointMissing(t *testing.T) {
	_, err := Builder(openrtb_ext.BidderKontext, config.Adapter{ExtraAdapterInfo: `{"partnerId":"hola-test","token":"test-token"}`}, testServer)
	assert.EqualError(t, err, "kontext: endpoint is not configured")
}

func TestSupplyChainDomainOverride(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderKontext, config.Adapter{
		Endpoint:         testEndpoint + "/",
		ExtraAdapterInfo: `{"partnerId":"hola-test","token":"test-token","asi":"example.com"}`,
	}, testServer)
	require.NoError(t, err)
	a := bidder.(*adapter)
	assert.Equal(t, "example.com", a.info.ASI)
	assert.Equal(t, testEndpoint, a.endpoint)
}

// Prebid.js can reject bids without advertiser domains in the bid metadata.
func TestMakeBidsSetsAdvertiserDomains(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderKontext, config.Adapter{
		Endpoint:         testEndpoint,
		ExtraAdapterInfo: `{"partnerId":"hola-test","token":"test-token"}`,
	}, testServer)
	require.NoError(t, err)

	request := &openrtb2.BidRequest{Imp: []openrtb2.Imp{{ID: "imp-1", Native: &openrtb2.Native{Request: "{}"}}}}
	body := `{"cur":"USD","seatbid":[{"seat":"1099","bid":[{"id":"bid-1","impid":"imp-1","price":1,"mtype":4,"adomain":["brand.example.com"],"adm":"{\"assets\":[]}"}]}]}`
	response, errs := bidder.MakeBids(request, nil, &adapters.ResponseData{StatusCode: 200, Body: []byte(body)})
	require.Empty(t, errs)
	require.Len(t, response.Bids, 1)
	require.NotNil(t, response.Bids[0].BidMeta)
	assert.Equal(t, []string{"brand.example.com"}, response.Bids[0].BidMeta.AdvertiserDomains)
}

// PBS shares the request between bidders, so appending our node must never
// touch the caller's source or its node slice.
func TestSupplyChainLeavesCallerSourceUnchanged(t *testing.T) {
	hp := int8(1)
	nodes := make([]openrtb2.SupplyChainNode, 1, 4) // spare capacity: a plain append would write into it
	nodes[0] = openrtb2.SupplyChainNode{ASI: "velocity.io", SID: "615cbf2d3872", HP: &hp}
	caller := &openrtb2.Source{TID: "t-1", SChain: &openrtb2.SupplyChain{Ver: "1.0", Complete: 1, Nodes: nodes}}

	out := sourceWithSupplyChain(caller, defaultASI, "52")

	require.Len(t, out.SChain.Nodes, 2)
	assert.Equal(t, "sayhola.ai", out.SChain.Nodes[1].ASI)
	assert.Equal(t, "52", out.SChain.Nodes[1].SID)
	assert.Equal(t, int8(1), out.SChain.Complete)
	assert.Len(t, caller.SChain.Nodes, 1)
	assert.Equal(t, openrtb2.SupplyChainNode{}, nodes[:2][1], "caller's backing array was written")
}
