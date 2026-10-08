package parental

import (
	"slices"
	"strings"
)

// RatingState is how a media module's classification of one item resolved
// (ADR-0031 Decision 2.5). The zero value is Unavailable so that an
// uninitialized Classification fails closed.
type RatingState int

const (
	// Unavailable means the rating is empty, an unknown token, the item is
	// missing, or the lookup failed. Restricted policies always deny it.
	Unavailable RatingState = iota
	// Rated means Classification.Rating holds a token on the supported ladder.
	Rated
	// Unrated means a source explicitly recorded NR/UR for the item.
	Unrated
)

// Classification is the trusted metadata for one item. It must come from the
// owning media module, never from request fields or user-writable data.
// TagsKnown is false when the tag lookup failed or was not performed.
type Classification struct {
	State     RatingState
	Rating    string
	Tags      []string
	TagsKnown bool
}

// Reason is a stable, machine-readable explanation of a Decision.
type Reason string

const (
	ReasonAllowed           Reason = "allowed"
	ReasonUnrestricted      Reason = "unrestricted"
	ReasonInvalidPolicy     Reason = "invalid_policy"
	ReasonRatingUnavailable Reason = "rating_unavailable"
	ReasonUnrated           Reason = "unrated"
	ReasonRatingAbove       Reason = "rating_above_limit"
	ReasonTagsUnknown       Reason = "tags_unknown"
	ReasonBlockedTag        Reason = "blocked_tag"
	ReasonAllowedTagMissing Reason = "allowed_tag_missing"
)

// Decision is the outcome of Evaluate. Allowed is true only for
// ReasonAllowed and ReasonUnrestricted.
type Decision struct {
	Allowed bool
	Reason  Reason
}

// kidsCeiling is the rating ceiling kids_mode applies when max_rating is empty.
const kidsCeiling = "PG"

// ratingLevels is one ordered ladder over exactly the tokens Normalize accepts
// for max_rating. It mirrors the SPA levels without NR/UR, which are states and
// not ratings. Unknown tokens (for example "15" or "12A") are not on it.
var ratingLevels = map[string]int{
	"G": 0, "TV-Y": 0, "TV-Y7": 0, "TV-Y7-FV": 0, "ALL": 0, "E": 0,
	"PG": 1, "TV-G": 1, "TV-PG": 1, "E10+": 1,
	"PG-13": 2, "TV-14": 2, "T": 2,
	"R": 3, "TV-MA": 3, "M": 3, "MA": 3,
	"NC-17": 4, "AO": 4, "X": 4,
}

// RatingLevel returns the ladder position of a rating token, ignoring case and
// surrounding space. ok is false for anything not on the ladder, including
// NR/UR and the empty string.
func RatingLevel(token string) (level int, ok bool) {
	level, ok = ratingLevels[strings.ToUpper(strings.TrimSpace(token))]
	return level, ok
}

// Evaluate decides whether a principal with policy p may see an item with
// classification c (ADR-0031 Decision 2.6, ADR-0030 item 6). It is pure,
// deterministic and total: it never panics, and any policy that does not
// validate (nil or unknown mode, bad version, bad ceiling) is denied.
//
// An unrestricted policy is allowed without consulting c. Otherwise the rating
// is checked first and tags second, so a tag match can never override a rating
// or unrated denial.
func Evaluate(p Policy, c Classification) Decision {
	p, err := Normalize(p)
	if err != nil {
		return deny(ReasonInvalidPolicy)
	}
	if p.Mode == "unrestricted" {
		return Decision{Allowed: true, Reason: ReasonUnrestricted}
	}
	rules := p.Rules

	switch c.State {
	case Rated:
		level, ok := RatingLevel(c.Rating)
		if !ok {
			return deny(ReasonRatingUnavailable)
		}
		ceiling := rules.MaxRating
		if ceiling == "" && rules.KidsMode {
			ceiling = kidsCeiling
		}
		if ceiling != "" {
			limit, ok := RatingLevel(ceiling)
			if !ok {
				return deny(ReasonInvalidPolicy)
			}
			if level > limit {
				return deny(ReasonRatingAbove)
			}
		}
	case Unrated:
		if !rules.AllowUnrated {
			return deny(ReasonUnrated)
		}
	default:
		return deny(ReasonRatingUnavailable)
	}

	if len(rules.BlockedTags) == 0 && len(rules.AllowedTags) == 0 {
		return Decision{Allowed: true, Reason: ReasonAllowed}
	}
	if !c.TagsKnown {
		return deny(ReasonTagsUnknown)
	}
	tags := make([]string, 0, len(c.Tags))
	for _, tag := range c.Tags {
		if tag = normalizeTag(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	for _, blocked := range rules.BlockedTags {
		if slices.Contains(tags, blocked) {
			return deny(ReasonBlockedTag)
		}
	}
	if len(rules.AllowedTags) > 0 && !slices.ContainsFunc(rules.AllowedTags, func(allowed string) bool {
		return slices.Contains(tags, allowed)
	}) {
		return deny(ReasonAllowedTagMissing)
	}
	return Decision{Allowed: true, Reason: ReasonAllowed}
}

func deny(reason Reason) Decision { return Decision{Reason: reason} }
