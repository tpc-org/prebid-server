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
//     app:<bundle> for auctions without one) is listed in its "labels",
//     or "labels" contains "*" (every auction, including DSP in-app
//     traffic without a placement) and none of its labels is in "exclude";
//   - that label is not listed in ivt-breaker.json (the circuit breaker the
//     hosts write when a placement's blocked share spikes);
//   - blockReason() returns a reason.
//
// blockReason mirrors pbs-settings nginx/ivt/ivt_enduser.py list_class():
// forwarded traffic without the end user's own device (unverifiable) is
// never blocked, nor is Apple Private Relay or a forwarder's own address.
// Behaviour blocks use the confirmed-bot hashes the hosts publish hourly
// (confirmed.txt, "<hk>:<iph>" per line), keyed on this week's hash key.
// UA-cluster blocks (confirmed-ua.json, {"clusters":[{hk,uah,label}]}) catch a
// bot spread over many IPs: one exact, outdated UA concentrated on one
// placement. Each entry only blocks that exact UA hash on that one label.
// The hosts publish entries only when ivt-enforce.json has "cluster":
// "enforce" (pbs-settings nginx/ivt ivtlib.ClusterPolicy).
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
	Mode    string   `json:"mode"`
	Labels  []string `json:"labels"`
	Exclude []string `json:"exclude"` // with "*": labels kept in monitor
}

type breakerConfig struct {
	Labels []string `json:"labels"`
}

type uaClusterConfig struct {
	Clusters []struct {
		HK    string `json:"hk"`
		UAH   string `json:"uah"`
		Label string `json:"label"`
	} `json:"clusters"`
}

// toUAClusters indexes confirmed-ua.json as "<hk>:<uah>" -> labels.
func toUAClusters(c uaClusterConfig) map[string]map[string]struct{} {
	out := map[string]map[string]struct{}{}
	for _, e := range c.Clusters {
		if e.HK == "" || e.UAH == "" || e.Label == "" {
			continue
		}
		k := e.HK + ":" + e.UAH
		if out[k] == nil {
			out[k] = map[string]struct{}{}
		}
		out[k][e.Label] = struct{}{}
	}
	return out
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
		l.enforceExclude = toSet(ec.Exclude)
	}
	var bc breakerConfig
	if data, err := os.ReadFile(filepath.Join(dir, "ivt-breaker.json")); err == nil && json.Unmarshal(data, &bc) == nil {
		l.breakerLabels = toSet(bc.Labels)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "confirmed.txt")); err == nil {
		l.confirmed = toSet(strings.Split(string(data), "\n"))
	}
	var uc uaClusterConfig
	if data, err := os.ReadFile(filepath.Join(dir, "confirmed-ua.json")); err == nil && json.Unmarshal(data, &uc) == nil {
		l.confirmedUA = toUAClusters(uc)
	}
}

// blockReason is the in-process verdict: the reason this auction would be
// blocked, or "" if it would not. Same order as ivt_enduser.list_class().
// labels are the auction's labels (auctionLabels); only UA-cluster entries
// use them, since each is scoped to one placement.
func blockReason(u *endUserLine, l *ivtLists, labels ...string) string {
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
	if (u.IPC == "dc" || u.IPC == "clean") && u.UAH != "" && u.HK != "" {
		if scoped, ok := l.confirmedUA[u.HK+":"+u.UAH]; ok {
			for _, label := range labels {
				if _, on := scoped[label]; on {
					return "behaviour-cluster"
				}
			}
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
	if _, all := l.enforceLabels["*"]; all {
		// Every auction, unless one of its labels is excluded or tripped.
		for _, label := range labels {
			if _, ex := l.enforceExclude[label]; ex {
				return false
			}
			if _, tripped := l.breakerLabels[label]; tripped {
				return false
			}
		}
		return true
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
