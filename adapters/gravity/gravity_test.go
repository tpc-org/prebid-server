package gravity

import (
	"encoding/json"
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

func testBuilder(t *testing.T) adapters.Bidder {
	t.Helper()
	bidder, err := Builder(openrtb_ext.BidderGravity, config.Adapter{
		Endpoint:         "https://server.trygravity.ai/api/v1/ad",
		ExtraAdapterInfo: `{"apiKey":"test-key","bidPrice":1.0}`,
	}, config.Server{})
	if err != nil {
		t.Fatalf("Builder returned unexpected error %v", err)
	}
	return bidder
}

func gravityImp(t *testing.T, ext map[string]interface{}) openrtb2.Imp {
	t.Helper()
	bidderExt, err := json.Marshal(map[string]interface{}{"bidder": ext})
	if err != nil {
		t.Fatalf("failed to marshal imp ext: %v", err)
	}
	return openrtb2.Imp{ID: "1", Ext: bidderExt}
}

func baseGravityExt(overrides map[string]interface{}) map[string]interface{} {
	ext := map[string]interface{}{
		"userId":      "user-1",
		"sessionId":   "session-1",
		"placement":   "below_response",
		"placementId": "sayhola-main",
		"messages":    []map[string]string{{"role": "user", "content": "hello"}},
	}
	for k, v := range overrides {
		ext[k] = v
	}
	return ext
}

// TestMakeRequestsSendsEmailHashNotHashedEmail locks in the 2026-08-10 fix:
// Gravity's real API (confirmed against their OpenAPI spec) expects
// "email_hash" in the user object, not "hashed_email" — the field name this
// adapter sent before the fix, which Gravity's API silently ignores rather
// than rejecting (unrecognized user-object fields are stored as generic
// context, not surfaced as an error), so this bug never showed up as a
// visible failure.
func TestMakeRequestsSendsEmailHashNotHashedEmail(t *testing.T) {
	bidder := testBuilder(t)
	imp := gravityImp(t, baseGravityExt(map[string]interface{}{
		"hashedEmail": "abc123hash",
	}))
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(reqData) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqData))
	}

	var body map[string]interface{}
	if err := json.Unmarshal(reqData[0].Body, &body); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	user, ok := body["user"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a user object in the request body, got %v", body["user"])
	}
	if got := user["email_hash"]; got != "abc123hash" {
		t.Errorf(`user.email_hash = %v, want "abc123hash"`, got)
	}
	if _, present := user["hashed_email"]; present {
		t.Errorf("user.hashed_email should not be present (that was the bug) — got %v", user["hashed_email"])
	}
}

// TestMakeRequestsOmitsEmailHashWhenNotProvided confirms the field stays
// genuinely optional — most publishers won't have a logged-in user's email
// available, and Gravity's own schema marks it optional.
func TestMakeRequestsOmitsEmailHashWhenNotProvided(t *testing.T) {
	bidder := testBuilder(t)
	imp := gravityImp(t, baseGravityExt(nil))
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(reqData[0].Body, &body); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	user := body["user"].(map[string]interface{})
	if _, present := user["email_hash"]; present {
		t.Errorf("user.email_hash should be omitted when not provided, got %v", user["email_hash"])
	}
}

// TestMakeRequestsSendsHashedPhone confirms the phone field (whose name was
// already correct) still works after the email field's fix — a regression
// check that the two fields weren't accidentally coupled.
func TestMakeRequestsSendsHashedPhone(t *testing.T) {
	bidder := testBuilder(t)
	imp := gravityImp(t, baseGravityExt(map[string]interface{}{
		"hashedPhone": "phone-hash-1",
	}))
	request := &openrtb2.BidRequest{ID: "req-1", Imp: []openrtb2.Imp{imp}}

	reqData, errs := bidder.MakeRequests(request, nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(reqData[0].Body, &body); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}
	user := body["user"].(map[string]interface{})
	if got := user["hashed_phone"]; got != "phone-hash-1" {
		t.Errorf(`user.hashed_phone = %v, want "phone-hash-1"`, got)
	}
}
