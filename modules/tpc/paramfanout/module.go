// Package paramfanout lets a server-side or Mobile SDK integration send one
// generic per-auction content block — ext.prebid.bidder.tpc: {userId,
// sessionId, chatId, messages, hashedEmail} — instead of knowing which real
// demand partners (thrad, imprezia, gravity) sit behind a placement, or
// what field each one's own API actually expects.
//
// This is the same job the web bundle's tpcBidAdapter.js imp() converter
// already does — turning window.tpc.data into partner-specific bidder ext
// — done server-side instead, for the two integration paths that have no
// browser-side bundle to do it in. The field-mapping table below is the
// single source of truth for that job; it should be the only place that
// knowledge lives (today it's also duplicated in the server-side and
// mobile SDK integration guides' hand-rolled buildBidderExt() samples —
// see docs/integration/server-side-integration.md and
// mobile-sdk-integration.md, which get rewritten to use this module
// instead once it's deployed and verified live).
//
// Runs at processed_auction_request — after the Stored Imp merge, before
// any bidder is called (see hooks/hookstage/processedauctionrequest.go) —
// so a bidder key (thrad/imprezia/gravity) is only ever filled in when the
// Stored Imp already provisioned that bidder for this imp. Swapping,
// adding, or removing a partner behind a placement stays a pbs-settings
// Stored Imp change only; this module never decides which partners are
// active, only how to fill in each one's dynamic fields once the Stored
// Imp says it's present.
//
// Strictly additive: a destination field that already has a non-empty
// value is never overwritten. That's what makes this safe to turn on
// without touching the web bundle or any existing hand-built server-side/
// mobile request — both already send fully-populated bidder-specific ext
// directly, so this module sees nothing to fill in and no-ops for them.
// A request with no ext.prebid.bidder.tpc block at all (the case for
// every non-native placement, and every request built before this module
// existed) is a no-op before that check even runs.
package paramfanout

import (
	"context"
	"encoding/json"

	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/modules/moduledeps"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// Builder reads the module's enabled flag once at PBS startup.
func Builder(cfg json.RawMessage, _ moduledeps.ModuleDeps) (interface{}, error) {
	c, err := newConfig(cfg)
	if err != nil {
		return nil, err
	}
	return Module{enabled: c.Enabled}, nil
}

type Module struct {
	enabled bool
}

// tpcGenericParams is the single generic block a server-side backend or
// Mobile SDK app sends under ext.prebid.bidder.tpc — the non-browser
// equivalent of the web bundle's window.tpc.data. Every field is optional;
// omit whatever a given placement's partners don't need.
type tpcGenericParams struct {
	UserID      string       `json:"userId,omitempty"`
	SessionID   string       `json:"sessionId,omitempty"`
	ChatID      string       `json:"chatId,omitempty"`
	HashedEmail string       `json:"hashedEmail,omitempty"`
	Messages    []tpcMessage `json:"messages,omitempty"`
}

type tpcMessage struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

func (p tpcGenericParams) lastByRole(role string) string {
	for i := len(p.Messages) - 1; i >= 0; i-- {
		if p.Messages[i].Role == role {
			return p.Messages[i].Content
		}
	}
	return ""
}

// HandleProcessedAuctionHook runs once per full auction, after the Stored
// Imp merge, before any bidder is called. It never rejects — a malformed
// or absent tpc/thrad/imprezia/gravity block is simply left as-is, the
// same "fail open, scoped to nothing" posture as an integration that never
// adopts the generic block at all.
func (m Module) HandleProcessedAuctionHook(
	_ context.Context,
	_ hookstage.ModuleInvocationContext,
	payload hookstage.ProcessedAuctionRequestPayload,
) (hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload], error) {
	result := hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload]{}
	if !m.enabled || payload.Request == nil || !anyImpHasGenericBlock(payload.Request) {
		return result, nil
	}

	result.ChangeSet.AddMutation(fanOutMutation, hookstage.MutationUpdate,
		"bidrequest", "imp", "ext", "prebid", "bidder")
	return result, nil
}

// anyImpHasGenericBlock is a cheap read-only pre-check so a request that
// never uses the generic "tpc" block — every existing web/hand-built
// request, and every non-native placement — registers no mutation at all,
// rather than one that always runs and decides internally to change
// nothing. Keeps this module's own hook metrics (nooped vs. updated)
// honest about how often it actually does anything.
func anyImpHasGenericBlock(req *openrtb_ext.RequestWrapper) bool {
	for _, impWrapper := range req.GetImp() {
		impExt, err := impWrapper.GetImpExt()
		if err != nil || impExt == nil {
			continue
		}
		if prebid := impExt.GetPrebid(); prebid != nil {
			if _, ok := prebid.Bidder["tpc"]; ok {
				return true
			}
		}
	}
	return false
}

// fanOutMutation is the ChangeSet mutation applied to every imp on the
// request — see hooks/hookstage/mutation.go and
// modules/prebid/rulesengine/hook_processed_auction.go for the same
// pattern used elsewhere in this codebase.
func fanOutMutation(
	p hookstage.ProcessedAuctionRequestPayload,
) (hookstage.ProcessedAuctionRequestPayload, error) {
	if p.Request == nil {
		return p, nil
	}

	for _, impWrapper := range p.Request.GetImp() {
		impExt, err := impWrapper.GetImpExt()
		if err != nil || impExt == nil {
			continue
		}
		impPrebid := impExt.GetPrebid()
		if impPrebid == nil || impPrebid.Bidder == nil {
			continue
		}

		tpcRaw, ok := impPrebid.Bidder["tpc"]
		if !ok {
			continue // no generic block on this imp — nothing to fan out
		}
		var generic tpcGenericParams
		if err := json.Unmarshal(tpcRaw, &generic); err != nil {
			continue // malformed generic block — leave everything else alone
		}

		changed := false
		if raw, ok := impPrebid.Bidder["thrad"]; ok {
			if merged, ok := fanOutThrad(raw, generic); ok {
				impPrebid.Bidder["thrad"] = merged
				changed = true
			}
		}
		if raw, ok := impPrebid.Bidder["imprezia"]; ok {
			if merged, ok := fanOutImprezia(raw, generic); ok {
				impPrebid.Bidder["imprezia"] = merged
				changed = true
			}
		}
		if raw, ok := impPrebid.Bidder["gravity"]; ok {
			if merged, ok := fanOutGravity(raw, generic); ok {
				impPrebid.Bidder["gravity"] = merged
				changed = true
			}
		}

		if changed {
			impExt.SetPrebid(impPrebid)
		}
	}

	return p, nil
}

// fanOutThrad fills in Thrad's dynamic UserID from the generic block,
// leaving every other field (including static PublisherID/RequestType/
// AdFormats already merged in from the Stored Imp) untouched. Only UserID
// is wired today — matching the mapping already verified live for the web
// bundle (see docs/integration/server-side-integration.md's Contextual
// section); Thrad's other dynamic fields (ChatID, Messages, Summary,
// TurnNumber) are defined on ExtImpThrads but not part of that verified
// mapping, so this deliberately does not touch them.
func fanOutThrad(raw json.RawMessage, generic tpcGenericParams) (json.RawMessage, bool) {
	var thrad openrtb_ext.ExtImpThrads
	if err := json.Unmarshal(raw, &thrad); err != nil {
		return raw, false
	}
	if thrad.UserID != "" || generic.UserID == "" {
		return raw, false // already set, or nothing to fill in — don't touch
	}
	thrad.UserID = generic.UserID
	merged, err := json.Marshal(thrad)
	if err != nil {
		return raw, false
	}
	return merged, true
}

// fanOutImprezia fills in Imprezia's dynamic Request/Response/SessionID
// from the generic block's messages/sessionId, matching the mapping
// already verified live for the web bundle: Request is the latest user
// turn, Response is the latest assistant turn (or a single-space
// placeholder — never empty, per Imprezia's own API rejecting `""`
// outright — see imp_imprezia.go's package doc), SessionID is required by
// Imprezia's real API even though our own schema marks it optional.
//
// KNOWN GAP (2026-08-23): imprezia.go's MakeRequests now also requires
// Timestamp and DeviceContext (added the same day, after Imprezia
// reported both missing from real traffic — see imp_imprezia.go's package
// doc) and soft-skips Imprezia when either is absent. Neither is filled
// in here — tpcGenericParams has no concept of either field today. A
// server-side/mobile integration relying solely on this module's fan-out
// (rather than sending fully-populated bidder ext itself) will silently
// get zero Imprezia bids until this is addressed — either by extending
// the generic block's schema with an optional deviceContext (Timestamp
// could reasonably default to time.Now() at fan-out time, but
// DeviceContext has no server-side source of truth) or by leaving it as
// documented caller responsibility. Not fixed here: this module has no
// confirmed real traffic yet (see package doc), same status as the
// Mobile SDK integration path itself.
func fanOutImprezia(raw json.RawMessage, generic tpcGenericParams) (json.RawMessage, bool) {
	var imprezia openrtb_ext.ExtImpImprezia
	if err := json.Unmarshal(raw, &imprezia); err != nil {
		return raw, false
	}

	changed := false
	if imprezia.Request == "" {
		if lastUser := generic.lastByRole("user"); lastUser != "" {
			imprezia.Request = lastUser
			changed = true
		}
	}
	if imprezia.Response == "" {
		if lastAssistant := generic.lastByRole("assistant"); lastAssistant != "" {
			imprezia.Response = lastAssistant
		} else {
			imprezia.Response = " " // placeholder until a reply exists — never ""
		}
		changed = true
	}
	if imprezia.SessionID == "" && generic.SessionID != "" {
		imprezia.SessionID = generic.SessionID
		changed = true
	}

	if !changed {
		return raw, false
	}
	merged, err := json.Marshal(imprezia)
	if err != nil {
		return raw, false
	}
	return merged, true
}

// fanOutGravity fills in Gravity's dynamic UserID/SessionID/HashedEmail
// from the generic block. Gravity is deactivated platform-wide as of
// 2026-08-18 (see docs/runbooks/gravity-reactivation.md), so no Stored Imp
// currently carries a "gravity" bidder key for this to act on — this
// exists for parity and for when Gravity is reactivated, and has NOT been
// verified against a live Gravity auction.
func fanOutGravity(raw json.RawMessage, generic tpcGenericParams) (json.RawMessage, bool) {
	var gravity openrtb_ext.ExtImpGravity
	if err := json.Unmarshal(raw, &gravity); err != nil {
		return raw, false
	}

	changed := false
	if gravity.UserID == "" && generic.UserID != "" {
		gravity.UserID = generic.UserID
		changed = true
	}
	if gravity.SessionID == "" && generic.SessionID != "" {
		gravity.SessionID = generic.SessionID
		changed = true
	}
	if gravity.HashedEmail == "" && generic.HashedEmail != "" {
		gravity.HashedEmail = generic.HashedEmail
		changed = true
	}

	if !changed {
		return raw, false
	}
	merged, err := json.Marshal(gravity)
	if err != nil {
		return raw, false
	}
	return merged, true
}
