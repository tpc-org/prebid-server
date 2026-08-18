// Package activitylog replaces the stock analytics/filesystem module as
// PBS's permanent, privacy-safe activity log — see
// docs/architecture/pbs-analytics-logging.md.
//
// ── Why a hook module, not an analytics.Module ────────────────────────────
//
// The stock filesystem module (analytics/filesystem) dumps the ENTIRE raw
// request+response for every auction, including real user-typed chat text
// (thrad.messages[].content) and device IPs, with no redaction option — a
// deliberate short-term tradeoff when first enabled 2026-08-07, no longer
// acceptable once the log became permanent (it now also backs the
// ad-request-count and bid-stats reporting pipelines). Registering a
// replacement analytics.Module would mean editing analytics/build/build.go,
// an upstream/vendor file. This module instead uses the pluggable
// modules/tpc/* hook system (same as modules/tpc/profanityfilter), needing
// zero upstream changes.
//
// ── Two correlated hook stages ────────────────────────────────────────────
//
// ProcessedAuctionRequest (after stored-imp merge, before any bidder is
// called) sees the full merged request: every imp's
// ext.prebid.storedrequest.id and which bidders are configured
// (ext.prebid.bidder map keys, excluding "tpc" — the client-side wrapper
// bidder code, not a real demand partner). AuctionResponse (end of
// processing) sees the final assembled BidResponse with every bidder's
// actual bids. Neither stage alone has both halves of what's needed for
// per-bidder request+response counts, so ProcessedAuctionRequest stashes a
// per-imp summary in hookstage.ModuleContext (confirmed via
// hooks/hookexecution/executor.go: moduleContexts is a field on the
// per-request hookExecutor, not shared/global state — safe under
// concurrent requests) and AuctionResponse reads it back to join with bid
// counts before writing one output line.
//
// ── Two output files, by design ───────────────────────────────────────────
//
// activity.log-YYYYMMDD: always written, one line per auction. Contains
// only stored_imp_id + per-bidder requested/bid counts — no message
// content, no IPs. This is the sole data source for both the ad-request-
// count and bid-stats ingestion pipelines (pbs-settings' ingest_activity.py).
//
// debug.log-YYYYMMDD: written only when a staff-controlled flag file is
// present and fresh (see isDebugActive) — full per-bidder raw ext content
// (including message text) and device IP for that one request. Never
// merged into activity.log, so it's always obvious which file might hold
// PII, and it gets a much shorter retention window in deploy.sh's
// retention cron.
//
// ── Known limitation ───────────────────────────────────────────────────────
//
// If profanityfilter (or any earlier processed_auction_request hook)
// rejects an auction, this module's own ProcessedAuctionRequest hook must
// run BEFORE it in pbs.yaml's hook_sequence to still record the ad-request
// count (matching the old raw-log behavior, which counted every auction
// regardless of downstream rejection) — bidders on that imp will show as
// "requested" with 0 bids, slightly overcounting bid-requests for the rare
// profanity-blocked case. Not worth the complexity to special-case further.
package activitylog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/modules/moduledeps"
)

const moduleContextKey = "activitylog"

// Builder reads the module's config once at PBS startup — see config.go.
func Builder(cfg json.RawMessage, _ moduledeps.ModuleDeps) (interface{}, error) {
	c, err := newConfig(cfg)
	if err != nil {
		return nil, err
	}
	if c.Enabled && c.LogDir == "" {
		return nil, fmt.Errorf("activitylog: log_dir is required when enabled")
	}
	return Module{cfg: c}, nil
}

type Module struct {
	cfg config
}

// ── Per-imp data stashed between stages ────────────────────────────────────

// impActivity is what ProcessedAuctionRequest knows about one imp, keyed by
// that imp's request-scoped OpenRTB ID (openrtb2.Imp.ID) — NOT the stable
// stored_imp_id, since that's what AuctionResponse needs to correlate
// against openrtb2.Bid.ImpID, which only ever contains the request-scoped
// ID, never the stored_imp_id.
type impActivity struct {
	StoredImpID    string
	BiddersInvoked []string
	// DebugExt holds each invoked bidder's raw ext.prebid.bidder.<name>
	// blob, only populated when isDebugActive was true for this request —
	// see the package doc's "known limitation" note on why this is decided
	// once, at this stage, not re-checked at AuctionResponse time.
	DebugExt map[string]json.RawMessage
}

type stashedActivity struct {
	Imps          map[string]impActivity // keyed by imp.ID
	DebugActive   bool
	DebugDeviceIP string
}

// ── ProcessedAuctionRequest stage ──────────────────────────────────────────

func (m Module) HandleProcessedAuctionHook(
	_ context.Context,
	_ hookstage.ModuleInvocationContext,
	payload hookstage.ProcessedAuctionRequestPayload,
) (hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload], error) {
	result := hookstage.HookResult[hookstage.ProcessedAuctionRequestPayload]{}
	if !m.cfg.Enabled || payload.Request == nil {
		return result, nil
	}

	debugActive := m.isDebugActive()

	imps := make(map[string]impActivity)
	for _, imp := range payload.Request.GetImp() {
		if imp.Imp == nil || imp.ID == "" {
			continue
		}
		impExt, err := imp.GetImpExt()
		if err != nil || impExt == nil {
			continue
		}
		prebid := impExt.GetPrebid()
		if prebid == nil {
			continue
		}

		var storedImpID string
		if prebid.StoredRequest != nil {
			storedImpID = prebid.StoredRequest.ID
		}

		activity := impActivity{StoredImpID: storedImpID}
		for bidder, raw := range prebid.Bidder {
			if bidder == "tpc" {
				continue // client-side wrapper bidder code, not a real demand partner
			}
			activity.BiddersInvoked = append(activity.BiddersInvoked, bidder)
			if debugActive {
				if activity.DebugExt == nil {
					activity.DebugExt = make(map[string]json.RawMessage)
				}
				activity.DebugExt[bidder] = raw
			}
		}

		// Still recorded even with zero bidders (e.g. a stub banner/video
		// imp pending Adform provisioning) — ad-request counting only ever
		// needed storedrequest.id, matching the old raw-log semantics.
		imps[imp.ID] = activity
	}

	if len(imps) == 0 {
		return result, nil
	}

	stashed := stashedActivity{Imps: imps, DebugActive: debugActive}
	if debugActive && payload.Request.Device != nil {
		if payload.Request.Device.IP != "" {
			stashed.DebugDeviceIP = payload.Request.Device.IP
		} else {
			stashed.DebugDeviceIP = payload.Request.Device.IPv6
		}
	}

	result.ModuleContext = hookstage.ModuleContext{moduleContextKey: stashed}
	return result, nil
}

// ── AuctionResponse stage ──────────────────────────────────────────────────

func (m Module) HandleAuctionResponseHook(
	_ context.Context,
	miCtx hookstage.ModuleInvocationContext,
	payload hookstage.AuctionResponsePayload,
) (hookstage.HookResult[hookstage.AuctionResponsePayload], error) {
	result := hookstage.HookResult[hookstage.AuctionResponsePayload]{}
	if !m.cfg.Enabled {
		return result, nil
	}

	raw, ok := miCtx.ModuleContext[moduleContextKey]
	if !ok {
		// No stashed data — either ProcessedAuctionRequest never ran for
		// this request (e.g. rejected by an earlier-in-sequence hook, see
		// package doc), or there were no imps worth recording. Clean no-op.
		return result, nil
	}
	stashed, ok := raw.(stashedActivity)
	if !ok {
		return result, nil
	}

	m.writeActivityLine(stashed, payload.BidResponse)
	if stashed.DebugActive {
		m.writeDebugLine(stashed, payload.BidResponse)
	}

	return result, nil
}

// ── Output construction ─────────────────────────────────────────────────────

type activityImpLine struct {
	StoredImpID string                    `json:"stored_imp_id"`
	Bidders     map[string]activityBidder `json:"bidders,omitempty"`
}

type activityBidder struct {
	Requested bool `json:"requested"`
	Bids      int  `json:"bids"`
}

type activityLine struct {
	Timestamp string            `json:"ts"`
	Imps      []activityImpLine `json:"imps"`
}

func bidCountsByImpAndSeat(resp *openrtb2.BidResponse) map[string]map[string]int {
	counts := make(map[string]map[string]int)
	if resp == nil {
		return counts
	}
	for _, seatBid := range resp.SeatBid {
		for _, bid := range seatBid.Bid {
			if counts[bid.ImpID] == nil {
				counts[bid.ImpID] = make(map[string]int)
			}
			counts[bid.ImpID][seatBid.Seat]++
		}
	}
	return counts
}

func (m Module) writeActivityLine(stashed stashedActivity, resp *openrtb2.BidResponse) {
	bidCounts := bidCountsByImpAndSeat(resp)

	line := activityLine{Timestamp: time.Now().UTC().Format(time.RFC3339)}
	for _, activity := range stashed.Imps {
		if activity.StoredImpID == "" {
			continue
		}
		impLine := activityImpLine{StoredImpID: activity.StoredImpID}
		if len(activity.BiddersInvoked) > 0 {
			impLine.Bidders = make(map[string]activityBidder, len(activity.BiddersInvoked))
			for _, bidder := range activity.BiddersInvoked {
				impLine.Bidders[bidder] = activityBidder{Requested: true}
			}
		}
		line.Imps = append(line.Imps, impLine)
	}

	// Bid counts are correlated by the imp's request-scoped ID, but the
	// output is keyed by stored_imp_id — join here rather than in the
	// struct above, since bidCounts is keyed by imp.ID.
	for impID, activity := range stashed.Imps {
		perSeat, ok := bidCounts[impID]
		if !ok {
			continue
		}
		for i := range line.Imps {
			if line.Imps[i].StoredImpID != activity.StoredImpID {
				continue
			}
			for seat, n := range perSeat {
				b := line.Imps[i].Bidders[seat]
				b.Requested = true // a bid came back even if the imp didn't declare this bidder (shouldn't happen, but don't drop real data)
				b.Bids = n
				if line.Imps[i].Bidders == nil {
					line.Imps[i].Bidders = make(map[string]activityBidder)
				}
				line.Imps[i].Bidders[seat] = b
			}
		}
	}

	m.appendLine("activity", line)
}

type debugLine struct {
	Timestamp string         `json:"ts"`
	DeviceIP  string         `json:"device_ip,omitempty"`
	Imps      []debugImpLine `json:"imps"`
}

type debugImpLine struct {
	StoredImpID string                     `json:"stored_imp_id"`
	BidderExt   map[string]json.RawMessage `json:"bidder_ext,omitempty"`
}

func (m Module) writeDebugLine(stashed stashedActivity, _ *openrtb2.BidResponse) {
	line := debugLine{Timestamp: time.Now().UTC().Format(time.RFC3339), DeviceIP: stashed.DebugDeviceIP}
	for _, activity := range stashed.Imps {
		if activity.StoredImpID == "" || len(activity.DebugExt) == 0 {
			continue
		}
		line.Imps = append(line.Imps, debugImpLine{StoredImpID: activity.StoredImpID, BidderExt: activity.DebugExt})
	}
	if len(line.Imps) == 0 {
		return
	}
	m.appendLine("debug", line)
}

// appendLine writes one JSON line to <LogDir>/<prefix>.log-YYYYMMDD.
// Simple open-append-close per call rather than a cached file handle —
// traffic volume at TPC's current scale makes the extra open() cost
// negligible, and it avoids managing file-handle lifetime/day-rollover
// across concurrent requests. O_APPEND writes of this size are atomic on
// POSIX, so concurrent auctions writing to the same file is safe.
func (m Module) appendLine(prefix string, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	path := filepath.Join(m.cfg.LogDir, fmt.Sprintf("%s.log-%s", prefix, time.Now().UTC().Format("20060102")))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

// isDebugActive checks the staff-controlled flag file: must exist AND have
// been touched within the last cfg.DebugWindowHours — auto-expiry so
// nobody can silently leave full-content logging on indefinitely (exactly
// what happened with the original "temporary" raw dump).
func (m Module) isDebugActive() bool {
	if m.cfg.DebugFlagFile == "" {
		return false
	}
	info, err := os.Stat(m.cfg.DebugFlagFile)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) <= time.Duration(m.cfg.DebugWindowHours)*time.Hour
}
