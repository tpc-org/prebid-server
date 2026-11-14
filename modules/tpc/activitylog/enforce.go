package activitylog

// ── IVT enforcement (HOL-143) — OFF unless explicitly enabled per placement ──
//
// Every auction gets a shadow verdict (`u.blk`, the reason it WOULD be
// blocked) whether or not enforcement is on, so the hosts' monitor and this
// in-process logic can be compared before anything is switched on. An
// auction is actually rejected (`u.enf`, NBR 502, no bidder called) only
// when ALL of these hold:
//
//   - ivt-enforce.json in ivt_dir has "mode": "enforce" (the global kill
//     switch: anything else, or a missing/unreadable file, means monitor);
//   - one of the auction's labels (a stored_imp_id, or site:<domain> /
//     app:<bundle> for auctions without one) is listed in its "labels";
//   - that label is not listed in ivt-breaker.json (the circuit breaker the
//     hosts write when a placement's blocked share spikes);
//   - blockReason() returns a reason.
//
// blockReason mirrors pbs-settings nginx/ivt/ivt_enduser.py list_class():
// forwarded traffic without the end user's own device (unverifiable) is
// never blocked, nor is Apple Private Relay or a forwarder's own address.
// Behaviour blocks use the confirmed-bot hashes the hosts publish hourly
// (confirmed.txt, "<hk>:<iph>" per line), keyed on this week's hash key.
// The forwarding server always gets a normal no-bid response; its
// connection is never refused. Fail-open: any missing file means no block.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// SuspectedIVT is the exchange-specific NoBidReason (openrtb3 reserves 500+;
// 501 is the profanity filter's ContentPolicyViolation).
const SuspectedIVT = 502

type enforceConfig struct {
	Mode   string   `json:"mode"`
	Labels []string `json:"labels"`
}

type breakerConfig struct {
	Labels []string `json:"labels"`
}

func toSet(items []string) map[string]struct{} {
	s := make(map[string]struct{}, len(items))
	for _, i := range items {
		if i = strings.TrimSpace(i); i != "" {
			s[i] = struct{}{}
		}
	}
	return s
}

// loadEnforcement reads the enforcement switch, the breaker and the
// confirmed-bot hashes. Any error leaves enforcement off for that part.
func loadEnforcement(dir string, l *ivtLists) {
	var ec enforceConfig
	if data, err := os.ReadFile(filepath.Join(dir, "ivt-enforce.json")); err == nil && json.Unmarshal(data, &ec) == nil {
		l.enforceOn = ec.Mode == "enforce"
		l.enforceLabels = toSet(ec.Labels)
	}
	var bc breakerConfig
	if data, err := os.ReadFile(filepath.Join(dir, "ivt-breaker.json")); err == nil && json.Unmarshal(data, &bc) == nil {
		l.breakerLabels = toSet(bc.Labels)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "confirmed.txt")); err == nil {
		l.confirmed = toSet(strings.Split(string(data), "\n"))
	}
}

// blockReason is the in-process verdict: the reason this auction would be
// blocked, or "" if it would not. Same order as ivt_enduser.list_class().
func blockReason(u *endUserLine, l *ivtLists) string {
	if u == nil || l == nil {
		return ""
	}
	if u.Fwd && (u.IPSrc == "header" || u.UASrc == "header") {
		return "" // unverifiable: an integration problem, never an automatic block
	}
	switch u.IPC {
	case "relay", "forwarder":
		return ""
	case "manual", "crawler":
		return u.IPC
	}
	if u.UAK == "automation" || u.UAK == "bad-ua" {
		return "ua"
	}
	if (u.IPC == "dc" || u.IPC == "clean") && u.IPH != "" && u.HK != "" {
		if _, ok := l.confirmed[u.HK+":"+u.IPH]; ok {
			return "behaviour-" + u.IPC
		}
	}
	return ""
}

// enforces reports whether any of the auction's labels is switched on and
// not tripped by the breaker.
func (l *ivtLists) enforces(labels []string) bool {
	if l == nil || !l.enforceOn {
		return false
	}
	for _, label := range labels {
		if _, on := l.enforceLabels[label]; !on {
			continue
		}
		if _, tripped := l.breakerLabels[label]; tripped {
			continue
		}
		return true
	}
	return false
}

// auctionLabels are the labels enforcement and reporting use for an auction:
// every stored_imp_id, plus site:<domain> / app:<bundle> from `u`.
func auctionLabels(imps map[string]impActivity, u *endUserLine) []string {
	var out []string
	for _, a := range imps {
		if a.StoredImpID != "" {
			out = append(out, a.StoredImpID)
		}
	}
	if u != nil {
		switch {
		case u.Bundle != "":
			out = append(out, "app:"+u.Bundle)
		case u.Dom != "":
			out = append(out, "site:"+u.Dom)
		}
	}
	return out
}
