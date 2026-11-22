package adgentek

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
		`{"slotId":"ca10d818-1795354687823"}`,
		`{"slotId":"ca10d818-1795354687823","messages":[{"role":"user","content":"hi"}],"sessionId":"chat-1"}`,
		`{"slotId":"s","messages":[]}`,
		`{"slotId":"s","surfaceType":"search","custom":{"sub_id":"promptessor"}}`,
	} {
		if err := validator.Validate(openrtb_ext.BidderAdgentek, json.RawMessage(p)); err != nil {
			t.Errorf("Schema rejected adgentek params: %s\n%v", p, err)
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
		`"s"`,
		`{"slotId":1}`,
		`{"messages":"hi"}`,
		`{"surfaceType":"banner"}`,
		`{"custom":{"depth":3}}`,
	} {
		if err := validator.Validate(openrtb_ext.BidderAdgentek, json.RawMessage(p)); err == nil {
			t.Errorf("Schema allowed unexpected params: %s", p)
		}
	}
}
