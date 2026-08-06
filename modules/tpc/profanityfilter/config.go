package profanityfilter

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/prebid/prebid-server/v4/util/jsonutil"
)

// config is the module's startup config, read once from pbs.yaml's
// hooks.modules.tpc.profanityfilter block (see pbs-settings/pbs.yaml) — the
// same free-form-JSON convention already used for adapters.thrad.extra_info.
// This is deliberately global/static, not per-account: see the plan notes on
// why a single platform-wide list is what's being built.
type config struct {
	Enabled bool     `json:"enabled"`
	Words   []string `json:"words"`
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
