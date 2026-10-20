package thradx

import (
	"testing"

	"github.com/prebid/prebid-server/v4/adapters/adapterstest"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

func TestJsonSamples(t *testing.T) {
	bidder, buildErr := Builder(openrtb_ext.BidderThradx, config.Adapter{
		Endpoint: "https://ssp.thrad.ai/api/v2/bid-request",
		ExtraAdapterInfo: `{"productionKey":"pk_test_production","stagingKey":"pk_test_staging",
			"publishers":{"drawify":{"productionKey":"pk_drawify_prod","stagingKey":"pk_drawify_staging"}}}`,
	}, config.Server{})

	if buildErr != nil {
		t.Fatalf("Builder returned unexpected error %v", buildErr)
	}

	adapterstest.RunJSONBidderTest(t, "thradxtest", bidder)
}

func TestBuilderRequiresKeys(t *testing.T) {
	_, err := Builder(openrtb_ext.BidderThradx, config.Adapter{
		Endpoint: "https://ssp.thrad.ai/api/v2/bid-request",
	}, config.Server{})
	if err == nil {
		t.Fatal("expected an error when extra_info has no keys")
	}
}
