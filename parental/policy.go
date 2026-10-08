// Package parental defines the authoritative account policy contract (ADR-0030)
// and the single shared evaluator for it (ADR-0031). The evaluator is a pure
// function over a policy and a caller-supplied classification; it performs no
// lookups and grants no access by itself. In particular, missing policy and
// unavailable classification must never mean unrestricted access.
package parental

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"unicode"
)

const Capability = "userdata.parental-policy.v1"
const MaxBodyBytes = 32 << 10

// Scope contains only identity resolved by the auth provider. Empty TenantID is
// the single-household scope, not a wildcard for every tenant.
type Scope struct {
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
}

type Policy struct {
	Version int    `json:"version"`
	Mode    string `json:"mode"`
	Rules   *Rules `json:"rules"`
}

// Rules uses exact case-normalized tags (never substring matching). Blocked tags
// win; allowed tags require a match without overriding rating/unrated rules.
// KidsMode supplies a PG ceiling when MaxRating is empty. Classifications that
// a provider cannot establish are unavailable, distinct from genuinely unrated.
type Rules struct {
	KidsMode     bool     `json:"kids_mode"`
	MaxRating    string   `json:"max_rating"`
	BlockedTags  []string `json:"blocked_tags"`
	AllowedTags  []string `json:"allowed_tags"`
	AllowUnrated bool     `json:"allow_unrated"`
}

type Document struct {
	Scope
	State     string  `json:"state"`
	Revision  int64   `json:"revision"`
	Policy    *Policy `json:"policy"`
	UpdatedAt string  `json:"updated_at,omitempty"`
}

type Update struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Policy           Policy `json:"policy"`
}

func Unconfigured(scope Scope) Document {
	return Document{Scope: scope, State: "unconfigured"}
}

// ValidateScope does not normalize identities or accept a client tenant override.
func ValidateScope(scope Scope) error {
	if !validIdentity(scope.UserID, false) || !validIdentity(scope.TenantID, true) {
		return errors.New("invalid policy scope")
	}
	return nil
}

func validIdentity(s string, emptyOK bool) bool {
	if s == "" {
		return emptyOK
	}
	return len(s) <= 256 && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
}

// Normalize validates the versioned schema and returns an independent canonical
// value. NR/UR and unknown strings are not supported rating ceilings.
func Normalize(p Policy) (Policy, error) {
	if p.Version != 1 {
		return Policy{}, errors.New("unsupported policy version")
	}
	switch p.Mode {
	case "unrestricted":
		if p.Rules != nil {
			return Policy{}, errors.New("unrestricted policy requires null rules")
		}
		return p, nil
	case "restricted":
		if p.Rules == nil {
			return Policy{}, errors.New("restricted policy requires rules")
		}
	default:
		return Policy{}, errors.New("invalid policy mode")
	}
	rules := *p.Rules
	rules.MaxRating = strings.ToUpper(strings.TrimSpace(rules.MaxRating))
	switch rules.MaxRating {
	case "", "G", "TV-Y", "TV-Y7", "TV-Y7-FV", "ALL", "E", "PG", "TV-G", "TV-PG", "E10+", "PG-13", "TV-14", "T", "R", "TV-MA", "M", "MA", "NC-17", "AO", "X":
	default:
		return Policy{}, errors.New("unsupported maximum rating")
	}
	var err error
	rules.BlockedTags, err = normalizeTags(rules.BlockedTags)
	if err != nil {
		return Policy{}, err
	}
	rules.AllowedTags, err = normalizeTags(rules.AllowedTags)
	if err != nil {
		return Policy{}, err
	}
	p.Rules = &rules
	return p, nil
}

// normalizeTag is the one tag comparison form shared by policy rules and by
// item classifications, so both sides of an exact match are folded identically.
func normalizeTag(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

func normalizeTags(tags []string) ([]string, error) {
	if len(tags) > 64 {
		return nil, errors.New("too many policy tags")
	}
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = normalizeTag(tag)
		if tag == "" || len(tag) > 128 || strings.ContainsFunc(tag, unicode.IsControl) {
			return nil, errors.New("invalid policy tag")
		}
		result = append(result, tag)
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

// DecodeUpdate accepts complete replacements only. Unknown/duplicate keys,
// implicit null/default values, and additional JSON documents are rejected.
// The HTTP handler bounds the body before calling this function.
func DecodeUpdate(raw []byte) (Update, error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return Update{}, err
	}
	if err := requireKeys(obj, "expected_revision", "policy"); err != nil {
		return Update{}, err
	}
	var revision int64
	if err := json.Unmarshal(obj["expected_revision"], &revision); err != nil || revision < 0 || revision == math.MaxInt64 {
		return Update{}, errors.New("invalid expected_revision")
	}
	policy, err := DecodePolicy(obj["policy"])
	if err != nil {
		return Update{}, err
	}
	return Update{ExpectedRevision: revision, Policy: policy}, nil
}

// DecodePolicy also validates persisted data: malformed rows are errors, never
// converted into an absent or unrestricted policy.
func DecodePolicy(raw []byte) (Policy, error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return Policy{}, err
	}
	if err := requireKeys(obj, "version", "mode", "rules"); err != nil {
		return Policy{}, err
	}
	if !bytes.Equal(bytes.TrimSpace(obj["rules"]), []byte("null")) {
		rules, err := decodeObject(obj["rules"])
		if err != nil {
			return Policy{}, err
		}
		if err := requireKeys(rules, "kids_mode", "max_rating", "blocked_tags", "allowed_tags", "allow_unrated"); err != nil {
			return Policy{}, err
		}
		for _, v := range rules {
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return Policy{}, errors.New("policy rules cannot be null")
			}
		}
	}
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return Policy{}, errors.New("invalid policy types")
	}
	return Normalize(p)
}

func requireKeys(obj map[string]json.RawMessage, keys ...string) error {
	if len(obj) != len(keys) {
		return errors.New("missing or unknown policy fields")
	}
	for _, key := range keys {
		if _, ok := obj[key]; !ok {
			return errors.New("missing or unknown policy fields")
		}
		if key != "rules" && bytes.Equal(bytes.TrimSpace(obj[key]), []byte("null")) {
			return errors.New("required policy field is null")
		}
	}
	return nil
}

// Decode each object one member at a time so duplicate fields cannot disappear
// during map decoding. Nested policy/rules objects are decoded the same way.
func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) > MaxBodyBytes {
		return nil, errors.New("policy is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("policy object required")
	}
	obj := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errors.New("invalid policy JSON")
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("invalid policy field")
		}
		if _, exists := obj[key]; exists {
			return nil, errors.New("duplicate policy field")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, errors.New("invalid policy JSON")
		}
		obj[key] = value
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errors.New("invalid policy JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after policy object")
	}
	return obj, nil
}
