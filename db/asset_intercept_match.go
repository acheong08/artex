package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Asset-intercept rule matching and enforcement. asset_intercept.go stores the rules;
// this file matches target asset domains/IPs/URLs against enabled rules. Agent tools
// (add_intent, insert_assets) call this before submitting intents or inserting assets;
// a match is rejected.

// AssetInterceptKindLabel returns a human-readable label for kind for agent messages.
func AssetInterceptKindLabel(kind string) string {
	switch kind {
	case "exact_domain":
		return "Exact domain"
	case "exact_ip":
		return "Exact IP"
	case "exact_url":
		return "Exact URL"
	case "fuzzy_domain":
		return "Fuzzy domain"
	case "fuzzy_ip":
		return "Fuzzy IP"
	case "fuzzy_url":
		return "Fuzzy URL"
	case "cidr":
		return "CIDR range"
	}
	return kind
}

// Reason returns a readable match reason, e.g. "Matched asset intercept rule [domain (fuzzy): .gov.cn] (note)".
func (r AssetInterceptRule) Reason() string {
	s := fmt.Sprintf("Matched asset interception rule [%s: %s]", AssetInterceptKindLabel(r.Kind), r.Pattern)
	if note := strings.TrimSpace(r.Note); note != "" {
		s += " (" + note + ")"
	}
	return s
}

// matchOne checks whether an enabled rule matches any candidate domain/IP/URL and returns the matched value.
func matchOne(r AssetInterceptRule, domains, ips, urls []string) (string, bool) {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return "", false
	}
	switch r.Kind {
	case "exact_domain":
		for _, d := range domains {
			if strings.EqualFold(strings.TrimSpace(d), p) {
				return d, true
			}
		}
	case "exact_ip":
		for _, ip := range ips {
			if strings.TrimSpace(ip) == p {
				return ip, true
			}
		}
	case "exact_url":
		for _, u := range urls {
			if strings.TrimSpace(u) == p {
				return u, true
			}
		}
	case "fuzzy_domain":
		lp := strings.ToLower(p)
		for _, d := range domains {
			if d != "" && strings.Contains(strings.ToLower(d), lp) {
				return d, true
			}
		}
	case "fuzzy_ip":
		for _, ip := range ips {
			if ip != "" && strings.Contains(ip, p) {
				return ip, true
			}
		}
	case "fuzzy_url":
		lp := strings.ToLower(p)
		for _, u := range urls {
			if u != "" && strings.Contains(strings.ToLower(u), lp) {
				return u, true
			}
		}
	case "cidr":
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return "", false
		}
		for _, ip := range ips {
			if pip := net.ParseIP(strings.TrimSpace(ip)); pip != nil && ipnet.Contains(pip) {
				return ip, true
			}
		}
	}
	return "", false
}

// MatchAssetInterceptRules returns the first enabled rule matching candidate domain/IP/URL strings,
// together with the matched value. insert_assets uses this on raw, not-yet-persisted assetInputItems.
func MatchAssetInterceptRules(rules []AssetInterceptRule, domains, ips, urls []string) (AssetInterceptRule, string, bool) {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if v, ok := matchOne(r, domains, ips, urls); ok {
			return r, v, true
		}
	}
	return AssetInterceptRule{}, "", false
}

// interceptCandidates extracts domain/IP/URL candidates from a persisted asset for intercept matching.
// It extracts and classifies the URL host so domain/IP rules also match service assets that have only a URL.
func (a *Asset) interceptCandidates() (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, a.Domain)
	add(&domains, a.RootDomain)
	for _, d := range a.BoundDomains {
		add(&domains, d)
	}
	add(&ips, a.IP)
	add(&urls, a.URL)
	if a.URL != "" {
		if u, err := url.Parse(a.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// InterceptLabel returns a short asset identifier for agent messages.
func (a *Asset) InterceptLabel() string {
	var target string
	switch {
	case a.Domain != "":
		target = a.Domain
	case a.URL != "":
		target = a.URL
	case a.IP != "":
		target = a.IP
	default:
		target = fmt.Sprintf("#%d", a.ID)
	}
	return fmt.Sprintf("Asset #%d[%s] %s", a.ID, a.Type, target)
}

// hasEnabledRule reports whether the rule set contains any enabled rule.
func hasEnabledRule(rules []AssetInterceptRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

// AssetGateDecision is the result of evaluating candidates through the "block, then allow" gate.
type AssetGateDecision struct {
	Allowed bool
	Reason  string // Rejection reason (without asset identifier); empty when Allowed=true
}

// EvaluateAssetGate applies task-level gate rules:
//  1. Any match in enabled blockRules => reject (blocked).
//  2. Otherwise, if allowRules have enabled entries and none match => reject (outside allowlist).
//  3. Otherwise => allow.
//
// If allowRules is empty or has no enabled entries, the allow gate is inactive (no allowlist;
// allow all) so that missing allow rules do not block every asset.
func EvaluateAssetGate(blockRules, allowRules []AssetInterceptRule, domains, ips, urls []string) AssetGateDecision {
	if rule, _, ok := MatchAssetInterceptRules(blockRules, domains, ips, urls); ok {
		return AssetGateDecision{Allowed: false, Reason: rule.Reason()}
	}
	if hasEnabledRule(allowRules) {
		if _, _, ok := MatchAssetInterceptRules(allowRules, domains, ips, urls); !ok {
			return AssetGateDecision{Allowed: false, Reason: "Outside the task's allowed (allowlist) scope; testing is not permitted"}
		}
	}
	return AssetGateDecision{Allowed: true}
}

// AssetInterceptHit describes an asset rejected by the gate (blocked or outside the allowlist).
type AssetInterceptHit struct {
	Asset  *Asset
	Reason string // Human-readable reason
}

// Describe returns a readable description combining asset information and the reason.
func (h AssetInterceptHit) Describe() string {
	return fmt.Sprintf("%s → %s", h.Asset.InterceptLabel(), h.Reason)
}

// ListAssetInterceptRules forwards to the matching *DB method so callers that only hold
// an AssetStore (such as agent tools) can read rules.
func (s *AssetStore) ListAssetInterceptRules() ([]AssetInterceptRule, error) {
	return s.db.ListAssetInterceptRules()
}

// CheckAssetsIntercept loads assets by ID, evaluates the "block, then allow" gate for each,
// and returns all rejected assets. Block rules = global ∪ task-level block; allow rules =
// task-level allow (this task only). Returns immediately for no IDs. Uses global GetByIDs
// (not filtered by task scope) so scope cannot weaken intercept rules.
func (s *AssetStore) CheckAssetsIntercept(taskID int64, ids []int64) ([]AssetInterceptHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	blockRules, err := s.db.ListAssetInterceptRules()
	if err != nil {
		return nil, err
	}
	var allowRules []AssetInterceptRule
	if taskID > 0 {
		tb, ta, err := s.TaskInterceptRulesSplit(taskID)
		if err != nil {
			return nil, err
		}
		blockRules = append(blockRules, tb...)
		allowRules = ta
	}
	// No block rules and no enabled allow rules => no gate to evaluate; allow all.
	if len(blockRules) == 0 && !hasEnabledRule(allowRules) {
		return nil, nil
	}
	assets, err := s.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	var hits []AssetInterceptHit
	for _, a := range assets {
		domains, ips, urls := a.interceptCandidates()
		if d := EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
			hits = append(hits, AssetInterceptHit{Asset: a, Reason: d.Reason})
		}
	}
	return hits, nil
}
