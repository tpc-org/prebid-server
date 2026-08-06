// Package profanityfilter rejects an entire auction, before any bidder is
// called, if the conversation text carried in a Thrad imp's
// ext.prebid.bidder.thrad.messages[].content contains a word from a
// configured banned-word list. See pbs-settings/pbs.yaml's
// hooks.modules.tpc.profanityfilter block for the actual word list and
// docs/architecture/profanity-filter.md for how to update it.
package profanityfilter

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/modules/moduledeps"
)

// ContentPolicyViolation is an exchange-specific NBR code (openrtb3's
// NoBidReason reserves 500+ for exchange-specific values) — there is no
// standard IAB reason for "the request itself is not fit to send to any
// demand partner", so this documents our own.
const ContentPolicyViolation = 501

// Builder reads the module's global word list once at PBS startup from
// pbs.yaml's hooks.modules.tpc.profanityfilter block and compiles it into
// regexes — no per-request parsing cost, no per-account config (see the
// package doc / plan notes for why a single platform-wide list is enough
// today).
func Builder(cfg json.RawMessage, _ moduledeps.ModuleDeps) (interface{}, error) {
	c, err := newConfig(cfg)
	if err != nil {
		return nil, err
	}
	return Module{
		enabled:  c.Enabled,
		patterns: compileWordPatterns(c.Words),
	}, nil
}

type Module struct {
	enabled  bool
	patterns []*regexp.Regexp
}

// thradBidderParams mirrors just the piece of adapters/thrad's ExtImpThrads
// this module needs — kept local rather than importing the adapter package,
// since all that's needed here is the messages' text content.
type thradBidderParams struct {
	Messages []struct {
		Content string `json:"content"`
	} `json:"messages"`
}

// HandleProcessedAuctionHook runs once per full auction, after request
// parsing, before any bidder is called (see hooks/hookstage/processedauctionrequest.go).
// A match in any imp's Thrad message content rejects the WHOLE auction —
// not just Thrad's bid — so no demand partner ever sees the request.
func (m Module) HandleProcessedAuctionHook(
	_ context.Context,
	_ hookstage.ModuleInvocationContext,
	payload hookstage.ProcessedAuctionRequestPayload,
) (hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload], error) {
	result := hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload]{}
	if !m.enabled || len(m.patterns) == 0 || payload.Request == nil {
		return result, nil
	}

	for _, imp := range payload.Request.GetImp() {
		impExt, err := imp.GetImpExt()
		if err != nil || impExt == nil {
			continue
		}
		prebid := impExt.GetPrebid()
		if prebid == nil {
			continue
		}
		raw, ok := prebid.Bidder["thrad"]
		if !ok {
			continue
		}

		var params thradBidderParams
		if err := json.Unmarshal(raw, &params); err != nil {
			continue
		}

		for _, message := range params.Messages {
			if m.containsBannedWord(message.Content) {
				result.Reject = true
				result.NbrCode = ContentPolicyViolation
				result.Message = "rejected: conversation content matched the profanity filter"
				return result, nil
			}
		}
	}

	return result, nil
}

func (m Module) containsBannedWord(text string) bool {
	for _, pattern := range m.patterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}
