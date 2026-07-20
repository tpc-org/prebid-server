package thrad

import (
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters/adapterstest"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

func TestJsonSamples(t *testing.T) {
	bidder, buildErr := Builder(openrtb_ext.BidderThrad, config.Adapter{
		Endpoint:         "https://ssp.thrads.ai/api/v1/ssp/bid-request",
		ExtraAdapterInfo: `{"productionKey":"pk_test_production","stagingKey":"pk_test_staging"}`,
	}, config.Server{})

	if buildErr != nil {
		t.Fatalf("Builder returned unexpected error %v", buildErr)
	}

	adapterstest.RunJSONBidderTest(t, "thradtest", bidder)
}

// TestSelectKeyPerPublisher locks in the per-publisher key selection added for
// Thrad's move to one Publisher (with its own key pair) per TPC publisher —
// including that today's flat, single-key pbs.yaml config (no "publishers"
// map at all) keeps working unchanged as the default/fallback.
func TestSelectKeyPerPublisher(t *testing.T) {
	extra := `{
		"productionKey": "pk_default_prod",
		"stagingKey":    "pk_default_staging",
		"publishers": {
			"drawify": {"productionKey": "pk_drawify_prod", "stagingKey": "pk_drawify_staging"}
		}
	}`
	bidder, err := Builder(openrtb_ext.BidderThrad, config.Adapter{
		Endpoint:         "https://ssp.thrads.ai/api/v1/ssp/bid-request",
		ExtraAdapterInfo: extra,
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	a := bidder.(*adapter)

	tests := []struct {
		name        string
		publisherID string
		test        int8
		want        string
	}{
		{"known publisher, production", "drawify", 0, "pk_drawify_prod"},
		{"known publisher, staging (test=1)", "drawify", 1, "pk_drawify_staging"},
		{"unknown publisher falls back to default", "learnrithm", 0, "pk_default_prod"},
		{"empty publisherId falls back to default", "", 0, "pk_default_prod"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := a.selectKey(&openrtb2.BidRequest{Test: tc.test}, tc.publisherID)
			if got != tc.want {
				t.Errorf("selectKey(publisherID=%q, test=%d) = %q, want %q", tc.publisherID, tc.test, got, tc.want)
			}
		})
	}
}

// TestBuilderAcceptsLegacyFlatExtraInfo confirms the exact flat shape already
// live in production pbs.yaml today ({"productionKey":...,"stagingKey":...},
// no "publishers" map) still builds and behaves identically to before this
// change — the Go deploy for this migration is a behavior no-op until
// pbs.yaml itself is edited to add per-publisher entries.
func TestBuilderAcceptsLegacyFlatExtraInfo(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderThrad, config.Adapter{
		Endpoint:         "https://ssp.thrads.ai/api/v1/ssp/bid-request",
		ExtraAdapterInfo: `{"productionKey":"pk_live_xxx","stagingKey":"pk_staging_yyy"}`,
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	a := bidder.(*adapter)

	if got := a.selectKey(&openrtb2.BidRequest{Test: 0}, "sayhola"); got != "pk_live_xxx" {
		t.Errorf("selectKey with no publishers map configured = %q, want %q (default production key)", got, "pk_live_xxx")
	}
	if got := a.selectKey(&openrtb2.BidRequest{Test: 1}, "sayhola"); got != "pk_staging_yyy" {
		t.Errorf("selectKey with no publishers map configured, test=1 = %q, want %q (default staging key)", got, "pk_staging_yyy")
	}
}
