package adsense

// This file tests static/bidder-params/adsense.json.
//
// These also validate the format of the external API:
// request.imp[i].ext.prebid.bidder.adsense

import (
	"encoding/json"
	"testing"

	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

func TestValidParams(t *testing.T) {
	validator, err := openrtb_ext.NewBidderParamsValidator("../../static/bidder-params")
	if err != nil {
		t.Fatalf("Failed to fetch the json-schemas. %v", err)
	}

	for _, validParam := range validParams {
		if err := validator.Validate(openrtb_ext.BidderAdsense, json.RawMessage(validParam)); err != nil {
			t.Errorf("Schema rejected adsense params: %s\n%v", validParam, err)
		}
	}
}

func TestInvalidParams(t *testing.T) {
	validator, err := openrtb_ext.NewBidderParamsValidator("../../static/bidder-params")
	if err != nil {
		t.Fatalf("Failed to fetch the json-schemas. %v", err)
	}

	for _, invalidParam := range invalidParams {
		if err := validator.Validate(openrtb_ext.BidderAdsense, json.RawMessage(invalidParam)); err == nil {
			t.Errorf("Schema allowed unexpected params: %s", invalidParam)
		}
	}
}

var validParams = []string{
	// Normal steady-state: no override, global extra_info.bidPrice applies.
	`{}`,
	// Per-imp bidPrice override, e.g. a hand-set launch price for a new publisher.
	`{"bidPrice":14.0}`,
}

var invalidParams = []string{
	// Wrong type for bidPrice.
	`{"bidPrice":"high"}`,
	// Malformed JSON.
	`{"bidPrice":1.0`,
}
