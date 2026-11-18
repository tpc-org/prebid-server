package kontext

import (
	"encoding/json"
	"testing"

	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// The schema must accept an empty object: PBS validates params before the
// adapter runs, and a rejected param set drops the whole imp for every bidder.
func TestValidParams(t *testing.T) {
	validator, err := openrtb_ext.NewBidderParamsValidator("../../static/bidder-params")
	if err != nil {
		t.Fatalf("Failed to fetch the json-schemas. %v", err)
	}
	for _, p := range []string{
		`{}`,
		`{"sellerid":"1"}`,
		`{"placementid":"sayhola-b8ed507b"}`,
		`{"sellerid":"1","placementid":"sayhola-b8ed507b"}`,
	} {
		if err := validator.Validate(openrtb_ext.BidderKontext, json.RawMessage(p)); err != nil {
			t.Errorf("Schema rejected kontext params: %s\n%v", p, err)
		}
	}
}

func TestInvalidParams(t *testing.T) {
	validator, err := openrtb_ext.NewBidderParamsValidator("../../static/bidder-params")
	if err != nil {
		t.Fatalf("Failed to fetch the json-schemas. %v", err)
	}
	for _, p := range []string{
		`null`,
		`"1"`,
		`{"sellerid":1}`,
		`{"placementid":["sayhola-b8ed507b"]}`,
	} {
		if err := validator.Validate(openrtb_ext.BidderKontext, json.RawMessage(p)); err == nil {
			t.Errorf("Schema allowed unexpected params: %s", p)
		}
	}
}
