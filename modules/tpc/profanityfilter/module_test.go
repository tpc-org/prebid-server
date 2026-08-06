package profanityfilter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/modules/moduledeps"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeWordsFile writes words as a flat JSON array to a temp file, mirroring
// the real pbs-settings/profanity_words.json format, and returns its path.
func writeWordsFile(t *testing.T, words []string) string {
	t.Helper()
	data, err := json.Marshal(words)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "words.json")
	require.NoError(t, os.WriteFile(path, data, 0644))
	return path
}

func buildModule(t *testing.T, enabled bool, words []string) Module {
	t.Helper()
	cfgMap := map[string]interface{}{"enabled": enabled}
	if words != nil {
		cfgMap["words_file"] = writeWordsFile(t, words)
	}
	cfg, err := json.Marshal(cfgMap)
	require.NoError(t, err)

	built, err := Builder(cfg, moduledeps.ModuleDeps{})
	require.NoError(t, err)
	module, ok := built.(Module)
	require.True(t, ok)
	return module
}

func testBuilder(t *testing.T, words []string) Module {
	t.Helper()
	return buildModule(t, true, words)
}

func thradImp(content string) openrtb2.Imp {
	extJSON, _ := json.Marshal(map[string]interface{}{
		"prebid": map[string]interface{}{
			"bidder": map[string]interface{}{
				"thrad": map[string]interface{}{
					"publisherId": "acme",
					"messages": []map[string]interface{}{
						{"role": "user", "content": content},
					},
				},
			},
		},
	})
	return openrtb2.Imp{ID: "1", Ext: extJSON}
}

func handle(t *testing.T, module Module, imps []openrtb2.Imp) hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload] {
	t.Helper()
	payload := hookstage.ProcessedAuctionRequestPayload{
		Request: &openrtb_ext.RequestWrapper{
			BidRequest: &openrtb2.BidRequest{ID: "test-auction", Imp: imps},
		},
	}
	result, err := module.HandleProcessedAuctionHook(context.Background(), hookstage.ModuleInvocationContext{}, payload)
	require.NoError(t, err)
	return result
}

func TestBuilderCompilesPatterns(t *testing.T) {
	module := testBuilder(t, []string{"fuck", "shit"})
	assert.True(t, module.enabled)
	assert.Len(t, module.patterns, 2)
}

func TestBuilderDisabledByDefault(t *testing.T) {
	built, err := Builder(json.RawMessage(`{}`), moduledeps.ModuleDeps{})
	require.NoError(t, err)
	module := built.(Module)
	assert.False(t, module.enabled)
}

func TestRejectsExactBannedWord(t *testing.T) {
	// Deliberately exact-word + simple-plural matching only (see the
	// package/plan notes) — "fucking"/"fucked" etc. are out of scope, this
	// tests the word itself, not an inflected form.
	module := testBuilder(t, []string{"fuck"})
	result := handle(t, module, []openrtb2.Imp{thradImp("what the fuck is this")})
	assert.True(t, result.Reject)
	assert.Equal(t, ContentPolicyViolation, result.NbrCode)
}

func TestRejectsSimplePlural(t *testing.T) {
	module := testBuilder(t, []string{"bastard"})
	result := handle(t, module, []openrtb2.Imp{thradImp("you bastards")})
	assert.True(t, result.Reject)
}

func TestRejectsEsPlural(t *testing.T) {
	module := testBuilder(t, []string{"ass"})
	result := handle(t, module, []openrtb2.Imp{thradImp("kick their asses")})
	assert.True(t, result.Reject)
}

func TestCaseInsensitive(t *testing.T) {
	module := testBuilder(t, []string{"fuck"})
	result := handle(t, module, []openrtb2.Imp{thradImp("FUCK this")})
	assert.True(t, result.Reject)
}

func TestDoesNotRejectScunthorpeStyleFalsePositive(t *testing.T) {
	// The classic profanity-filter false positive: "Scunthorpe" contains
	// "cunt" as a substring, but there's no word boundary between "cunt"
	// and the "h" that follows it inside the word, so \bcunt\b must not match.
	module := testBuilder(t, []string{"cunt"})
	result := handle(t, module, []openrtb2.Imp{thradImp("I'm looking for shoe shops in Scunthorpe")})
	assert.False(t, result.Reject)
}

func TestDoesNotRejectPenistoneOrCockburn(t *testing.T) {
	module := testBuilder(t, []string{"penis", "cock"})
	result := handle(t, module, []openrtb2.Imp{thradImp("Penistone and Cockburn are real English place names")})
	assert.False(t, result.Reject)
}

func TestDoesNotRejectUnrelatedNeighborWord(t *testing.T) {
	// "truck" must not be caught by a "fuck" pattern.
	module := testBuilder(t, []string{"fuck"})
	result := handle(t, module, []openrtb2.Imp{thradImp("I need a new fire truck toy")})
	assert.False(t, result.Reject)
}

func TestCleanContentPasses(t *testing.T) {
	module := testBuilder(t, []string{"fuck", "shit", "cunt"})
	result := handle(t, module, []openrtb2.Imp{thradImp("I'm looking for new running shoes")})
	assert.False(t, result.Reject)
}

func TestNoMessagesFieldIsCleanNoOp(t *testing.T) {
	module := testBuilder(t, []string{"fuck"})
	extJSON, _ := json.Marshal(map[string]interface{}{
		"prebid": map[string]interface{}{
			"bidder": map[string]interface{}{
				"gravity": map[string]interface{}{"placementId": "acme-main"},
			},
		},
	})
	result := handle(t, module, []openrtb2.Imp{{ID: "1", Ext: extJSON}})
	assert.False(t, result.Reject)
}

func TestEmptyImpExtIsCleanNoOp(t *testing.T) {
	module := testBuilder(t, []string{"fuck"})
	result := handle(t, module, []openrtb2.Imp{{ID: "1"}})
	assert.False(t, result.Reject)
}

func TestChecksEveryImpNotJustFirst(t *testing.T) {
	module := testBuilder(t, []string{"fuck"})
	imps := []openrtb2.Imp{thradImp("clean message")}
	second := thradImp("what the fuck")
	second.ID = "2"
	imps = append(imps, second)

	result := handle(t, module, imps)
	assert.True(t, result.Reject)
}

func TestDisabledModuleNeverRejects(t *testing.T) {
	module := buildModule(t, false, []string{"fuck"})
	result := handle(t, module, []openrtb2.Imp{thradImp("this is fucking great")})
	assert.False(t, result.Reject)
}

func TestBuilderErrorsOnMissingWordsFile(t *testing.T) {
	cfg, err := json.Marshal(map[string]interface{}{
		"enabled":    true,
		"words_file": filepath.Join(t.TempDir(), "does-not-exist.json"),
	})
	require.NoError(t, err)
	_, err = Builder(cfg, moduledeps.ModuleDeps{})
	assert.Error(t, err)
}

func TestBuilderErrorsOnMalformedWordsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "words.json")
	require.NoError(t, os.WriteFile(path, []byte("not valid json"), 0644))
	cfg, err := json.Marshal(map[string]interface{}{
		"enabled":    true,
		"words_file": path,
	})
	require.NoError(t, err)
	_, err = Builder(cfg, moduledeps.ModuleDeps{})
	assert.Error(t, err)
}
