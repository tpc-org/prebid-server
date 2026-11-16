package kontext

import (
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/adapters/adapterstest"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

const testEndpoint = "https://rtb.kontext.ai/openrtb2/bid"

func TestJsonSamples(t *testing.T) {
	bidder, err := Builder(openrtb_ext.BidderKontext, config.Adapter{
		Endpoint:         testEndpoint,
		ExtraAdapterInfo: `{"partnerId":"hola-test","token":"test-token"}`,
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	adapterstest.RunJSONBidderTest(t, "kontexttest", bidder)
}

// A Builder error stops PBS at boot, and the image ships before Kontext issues
// credentials. Missing or unreadable extra_info must leave an adapter that
// builds and sends nothing.
func TestUnconfiguredAdapterBuildsAndSendsNothing(t *testing.T) {
	request := &openrtb2.BidRequest{
		ID:   "req-1",
		Site: &openrtb2.Site{Domain: "sayhola.ai"},
		Imp:  []openrtb2.Imp{{ID: "imp-1", Native: &openrtb2.Native{Request: "{}"}}},
	}
	for name, extraInfo := range map[string]string{
		"no extra_info":         "",
		"unreadable extra_info": `{"partnerId":`,
		"token missing":         `{"partnerId":"hola-test"}`,
		"partner id missing":    `{"token":"test-token"}`,
	} {
		t.Run(name, func(t *testing.T) {
			bidder, err := Builder(openrtb_ext.BidderKontext, config.Adapter{Endpoint: testEndpoint, ExtraAdapterInfo: extraInfo}, config.Server{})
			if err != nil {
				t.Fatalf("Builder must not fail, got %v", err)
			}
			requests, errs := bidder.MakeRequests(request, &adapters.ExtraRequestInfo{})
			if len(requests) != 0 {
				t.Errorf("expected no outbound request, got %d", len(requests))
			}
			if len(errs) != 1 {
				t.Errorf("expected one error, got %v", errs)
			}
		})
	}
}
