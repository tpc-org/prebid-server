package thrad

import (
	"strings"
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters/adapterstest"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

func TestJsonSamples(t *testing.T) {
	bidder, buildErr := Builder(openrtb_ext.BidderThrad, config.Adapter{
		Endpoint: "https://ssp.thrad.ai/api/v2/bid-request",
		ExtraAdapterInfo: `{"productionKey":"pk_test_production","stagingKey":"pk_test_staging",
			"publishers":{"drawify":{"productionKey":"pk_drawify_prod","stagingKey":"pk_drawify_staging"}}}`,
	}, config.Server{})

	if buildErr != nil {
		t.Fatalf("Builder returned unexpected error %v", buildErr)
	}

	adapterstest.RunJSONBidderTest(t, "thradtest", bidder)
}

// TestJsonSamplesV2 runs the v2 fixtures through the "thradx" alias, whose
// default protocol is v2. (thradtest/supplemental/protocol-2-routes-to-v2.json
// covers "thrad" with an explicit "protocol": 2.)
func TestJsonSamplesV2(t *testing.T) {
	bidder, buildErr := Builder(openrtb_ext.BidderName("thradx"), config.Adapter{
		Endpoint: "https://ssp.thrad.ai/api/v2/bid-request",
		ExtraAdapterInfo: `{"productionKey":"pk_test_production","stagingKey":"pk_test_staging",
			"publishers":{"drawify":{"productionKey":"pk_drawify_prod","stagingKey":"pk_drawify_staging"}}}`,
	}, config.Server{})
	if buildErr != nil {
		t.Fatalf("Builder returned unexpected error %v", buildErr)
	}
	adapterstest.RunJSONBidderTest(t, "thradtestv2", bidder)
}

func TestProtocolSelection(t *testing.T) {
	extra := `{"productionKey":"pk"}`
	thrad, _ := Builder(openrtb_ext.BidderThrad, config.Adapter{Endpoint: "https://ssp.thrad.ai/api/v2/bid-request", ExtraAdapterInfo: extra}, config.Server{})
	thradx, _ := Builder(openrtb_ext.BidderName("thradx"), config.Adapter{Endpoint: "https://ssp.thrad.ai/api/v2/bid-request", ExtraAdapterInfo: extra}, config.Server{})
	for _, tc := range []struct {
		name     string
		a        *adapter
		protocol int
		want     int
	}{
		{"thrad default is v1", thrad.(*adapter), 0, 1},
		{"thrad opts in to v2", thrad.(*adapter), 2, 2},
		{"thradx default is v2", thradx.(*adapter), 0, 2},
		{"thradx can be pinned to v1", thradx.(*adapter), 1, 1},
		{"unknown value falls back to default", thrad.(*adapter), 7, 1},
	} {
		got := tc.a.protocolFor(openrtb_ext.ExtImpThrads{Protocol: tc.protocol})
		if got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestBuilderRequiresKeys(t *testing.T) {
	_, err := Builder(openrtb_ext.BidderThrad, config.Adapter{
		Endpoint: "https://ssp.thrad.ai/api/v2/bid-request",
	}, config.Server{})
	if err == nil {
		t.Fatal("expected an error when extra_info has no keys")
	}
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
		Endpoint:         "https://ssp.thrad.ai/api/v2/bid-request",
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

// TestSelectKeyStagingOnlyPublisherFallsBackToDefaultProduction locks in the
// 2026-07-25 fix: real (non-test) traffic for a publisher with only a
// staging key configured (learnrithm/drawify/slashspace's actual state at
// the time — their staging keys were confirmed returning 500s from Thrad,
// while sayhola's default production key works fine) must fall back to the
// shared default productionKey, not that publisher's own (unverified/broken)
// stagingKey. test=1 debugging still exercises that publisher's own staging
// key, unchanged.
func TestSelectKeyStagingOnlyPublisherFallsBackToDefaultProduction(t *testing.T) {
	extra := `{
		"productionKey": "pk_default_prod",
		"stagingKey":    "pk_default_staging",
		"publishers": {
			"learnrithm": {"stagingKey": "pk_learnrithm_staging"}
		}
	}`
	bidder, err := Builder(openrtb_ext.BidderThrad, config.Adapter{
		Endpoint:         "https://ssp.thrad.ai/api/v2/bid-request",
		ExtraAdapterInfo: extra,
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	a := bidder.(*adapter)

	if got := a.selectKey(&openrtb2.BidRequest{Test: 0}, "learnrithm"); got != "pk_default_prod" {
		t.Errorf("real traffic for staging-only publisher = %q, want %q (default production key)", got, "pk_default_prod")
	}
	if got := a.selectKey(&openrtb2.BidRequest{Test: 1}, "learnrithm"); got != "pk_learnrithm_staging" {
		t.Errorf("test=1 for staging-only publisher = %q, want %q (that publisher's own staging key, for debugging)", got, "pk_learnrithm_staging")
	}
}

// TestBuildUserHashedEmail: Thrad v2 refuses anything but a lowercase
// 64-char hex hash, and a refused field 400s the whole request, so only a
// valid hash is ever sent.
func TestBuildUserHashedEmail(t *testing.T) {
	hash := "6db61e6dcbcf2390e4a46af426f26a133a3bee45021422fc7ae86e9136f14110"
	tests := []struct {
		name   string
		params openrtb_ext.ExtImpThrads
		want   []string
	}{
		{"valid hash", openrtb_ext.ExtImpThrads{UserID: "u1", HashedEmail: hash}, []string{hash}},
		{"uppercase hex is lowered", openrtb_ext.ExtImpThrads{HashedEmail: strings.ToUpper(hash)}, []string{hash}},
		{"plain email dropped", openrtb_ext.ExtImpThrads{UserID: "u1", HashedEmail: "someone@example.com"}, nil},
		{"wrong length dropped", openrtb_ext.ExtImpThrads{UserID: "u1", HashedEmail: hash[:40]}, nil},
		{"absent", openrtb_ext.ExtImpThrads{UserID: "u1"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user := buildUser(tc.params)
			if user == nil {
				t.Fatal("buildUser returned nil; every case has a user id or a hash")
			}
			var got []string
			if user.ExtendedIDs != nil {
				got = user.ExtendedIDs.EmailsSHA256
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("emails_sha256 = %v, want %v", got, tc.want)
			}
		})
	}
	if buildUser(openrtb_ext.ExtImpThrads{HashedEmail: "not-a-hash"}) != nil {
		t.Error("a user block with nothing valid in it must be omitted")
	}
}

// TestOpportunityIDKind locks in the placement split Thrad asked for
// (2026-10-31): the stable placement id carries ".opener" before the
// conversation and ".contextual" once a chat context is sent.
func TestOpportunityIDKind(t *testing.T) {
	long := strings.Repeat("x", maxIDLen-len(".opener")+1)
	tests := []struct {
		name       string
		params     openrtb_ext.ExtImpThrads
		contextual bool
		want       string
	}{
		{"stored imp opener", openrtb_ext.ExtImpThrads{PlacementID: "sayhola-9243e9b6", PublisherID: "sayhola"}, false, "sayhola-9243e9b6.opener"},
		{"stored imp contextual", openrtb_ext.ExtImpThrads{PlacementID: "sayhola-9243e9b6", PublisherID: "sayhola"}, true, "sayhola-9243e9b6.contextual"},
		{"publisher fallback", openrtb_ext.ExtImpThrads{PublisherID: "magneta"}, true, "magneta.contextual"},
		{"imp id fallback", openrtb_ext.ExtImpThrads{}, false, "imp-1.opener"},
		{"suffix dropped past the length limit", openrtb_ext.ExtImpThrads{PlacementID: long}, false, long},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := opportunityID(openrtb2.Imp{ID: "imp-1"}, tc.params, tc.contextual); got != tc.want {
				t.Errorf("opportunityID = %q, want %q", got, tc.want)
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
		Endpoint:         "https://ssp.thrad.ai/api/v2/bid-request",
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
