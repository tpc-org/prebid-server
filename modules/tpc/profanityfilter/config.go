package profanityfilter

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"github.com/prebid/prebid-server/v4/util/jsonutil"
)

// config is the module's startup config, read once from pbs.yaml's
// hooks.modules.tpc.profanityfilter block (see pbs-settings/pbs.yaml) — the
// same free-form-JSON convention already used for adapters.thrad.extra_info.
// The word list itself is NOT inline here — WordsFile points at a separate
// JSON file, the same "pbs.yaml holds a pointer, content lives in
// pbs-settings' filesystem" pattern PBS's own stored_requests/stored_imps
// use, so growing the list is a pbs-settings-only file edit, no pbs.yaml
// diff. This is deliberately global/static, not per-account: see the plan
// notes on why a single platform-wide list is what's being built.
type config struct {
	Enabled   bool   `json:"enabled"`
	WordsFile string `json:"words_file"`
}

func newConfig(data json.RawMessage) (config, error) {
	var cfg config
	if len(data) == 0 {
		return cfg, nil
	}
	if err := jsonutil.UnmarshalValid(data, &cfg); err != nil {
		return cfg, fmt.Errorf("profanityfilter: failed to parse config: %s", err)
	}
	return cfg, nil
}

// loadWords reads the banned-word list from a flat JSON array file, e.g.
// ["fuck", "shit", ...] (see pbs-settings/profanity_words.json). An empty
// path is a clean no-op (zero words) — matches the module's existing
// "unconfigured is silent, not an error" convention. A configured-but-
// unreadable-or-malformed file fails loudly (Builder returns an error,
// which aborts PBS startup) rather than silently running with an inert
// filter — see docs/architecture/profanity-filter.md.
func loadWords(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("profanityfilter: failed to read words_file %q: %s", path, err)
	}
	var words []string
	if err := jsonutil.UnmarshalValid(data, &words); err != nil {
		return nil, fmt.Errorf("profanityfilter: failed to parse words_file %q: %s", path, err)
	}
	return words, nil
}

// compileWordPatterns builds one case-insensitive, word-boundary-anchored
// regex per configured word, with an optional "s"/"es" suffix for simple
// plurals. Word-boundary anchoring on both sides is what actually prevents
// false positives on words that merely CONTAIN a banned word (e.g.
// "Scunthorpe" contains "cunt", but there's no word boundary between "cunt"
// and the "h" that follows it inside "Scunthorpe", so \bcunt\b never matches
// there) — this isn't a hand-maintained exception list, it falls out of
// anchoring on real word boundaries instead of doing substring matching.
func compileWordPatterns(words []string) []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(words))
	for _, word := range words {
		if word == "" {
			continue
		}
		pattern := `(?i)\b` + regexp.QuoteMeta(word) + `(?:s|es)?\b`
		patterns = append(patterns, regexp.MustCompile(pattern))
	}
	return patterns
}
