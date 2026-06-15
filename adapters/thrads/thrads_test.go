package thrads

import (
	"testing"

	"github.com/prebid/prebid-server/v4/adapters/adapterstest"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

func TestJsonSamples(t *testing.T) {
	bidder, buildErr := Builder(openrtb_ext.BidderThrads, config.Adapter{
		Endpoint:         "https://ssp.thrads.ai/api/v1/ssp/bid-request",
		ExtraAdapterInfo: `{"productionKey":"pk_test_production","stagingKey":"pk_test_staging"}`,
	}, config.Server{})

	if buildErr != nil {
		t.Fatalf("Builder returned unexpected error %v", buildErr)
	}

	adapterstest.RunJSONBidderTest(t, "thradstest", bidder)
}
