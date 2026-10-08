package parental

import (
	"slices"
	"testing"
)

func restricted(r Rules) Policy {
	return Policy{Version: 1, Mode: "restricted", Rules: &r}
}

var unrestrictedPolicy = Policy{Version: 1, Mode: "unrestricted"}

func rated(token string, tags ...string) Classification {
	return Classification{State: Rated, Rating: token, Tags: tags, TagsKnown: true}
}

func TestRatingLevelLadder(t *testing.T) {
	order := [][]string{
		{"G", "TV-Y", "TV-Y7", "TV-Y7-FV", "ALL", "E"},
		{"PG", "TV-G", "TV-PG", "E10+"},
		{"PG-13", "TV-14", "T"},
		{"R", "TV-MA", "M", "MA"},
		{"NC-17", "AO", "X"},
	}
	for want, tokens := range order {
		for _, tok := range tokens {
			if got, ok := RatingLevel(tok); !ok || got != want {
				t.Errorf("RatingLevel(%q) = %d, %v; want %d, true", tok, got, ok, want)
			}
		}
	}
	if got, ok := RatingLevel(" pg-13 "); !ok || got != 2 {
		t.Errorf("case/space folding: %d %v", got, ok)
	}
	for _, tok := range []string{"", " ", "NR", "UR", "15", "12A", "PG13", "BANANAS", "PG-13 extra"} {
		if _, ok := RatingLevel(tok); ok {
			t.Errorf("RatingLevel(%q) is on the ladder", tok)
		}
	}
}

// Every token Normalize accepts as a ceiling must be on the ladder, so the
// two lists cannot drift apart.
func TestLadderCoversEverySupportedCeiling(t *testing.T) {
	for _, tok := range []string{"G", "TV-Y", "TV-Y7", "TV-Y7-FV", "ALL", "E", "PG", "TV-G", "TV-PG", "E10+", "PG-13", "TV-14", "T", "R", "TV-MA", "M", "MA", "NC-17", "AO", "X"} {
		if _, err := Normalize(restricted(Rules{MaxRating: tok})); err != nil {
			t.Fatalf("Normalize rejects %q: %v", tok, err)
		}
		if _, ok := RatingLevel(tok); !ok {
			t.Errorf("supported ceiling %q missing from ladder", tok)
		}
	}
	if len(ratingLevels) != 20 {
		t.Errorf("ladder has %d tokens, want 20", len(ratingLevels))
	}
}

func TestEvaluate(t *testing.T) {
	unrated := Classification{State: Unrated, Rating: "NR"}
	cases := []struct {
		name   string
		policy Policy
		class  Classification
		want   Decision
	}{
		// Unrestricted needs no classification.
		{"unrestricted zero classification", unrestrictedPolicy, Classification{}, Decision{true, ReasonUnrestricted}},
		{"unrestricted garbage classification", unrestrictedPolicy, Classification{State: RatingState(99), Rating: "\x00??", Tags: []string{"gore", ""}}, Decision{true, ReasonUnrestricted}},
		{"unrestricted adult", unrestrictedPolicy, rated("X"), Decision{true, ReasonUnrestricted}},

		// Invalid policies fail closed.
		{"zero policy", Policy{}, rated("G"), Decision{false, ReasonInvalidPolicy}},
		{"empty mode", Policy{Version: 1}, rated("G"), Decision{false, ReasonInvalidPolicy}},
		{"unknown mode", Policy{Version: 1, Mode: "open"}, rated("G"), Decision{false, ReasonInvalidPolicy}},
		{"wrong version", Policy{Version: 2, Mode: "unrestricted"}, rated("G"), Decision{false, ReasonInvalidPolicy}},
		{"restricted nil rules", Policy{Version: 1, Mode: "restricted"}, rated("G"), Decision{false, ReasonInvalidPolicy}},
		{"unrestricted with rules", Policy{Version: 1, Mode: "unrestricted", Rules: &Rules{}}, rated("G"), Decision{false, ReasonInvalidPolicy}},
		{"unsupported ceiling", restricted(Rules{MaxRating: "NR"}), rated("G"), Decision{false, ReasonInvalidPolicy}},

		// Restricted with no rules at all: rated items pass, others follow state.
		{"no ceiling rated passes", restricted(Rules{}), rated("NC-17"), Decision{true, ReasonAllowed}},
		{"zero classification denied", restricted(Rules{}), Classification{}, Decision{false, ReasonRatingUnavailable}},

		// Unavailable is always denied (ADR-0031 test 5).
		{"unavailable allow_unrated false", restricted(Rules{}), Classification{State: Unavailable}, Decision{false, ReasonRatingUnavailable}},
		{"unavailable allow_unrated true", restricted(Rules{AllowUnrated: true}), Classification{State: Unavailable}, Decision{false, ReasonRatingUnavailable}},
		{"unavailable carries a rating string", restricted(Rules{AllowUnrated: true}), Classification{State: Unavailable, Rating: "G", TagsKnown: true}, Decision{false, ReasonRatingUnavailable}},
		{"undefined state", restricted(Rules{AllowUnrated: true}), Classification{State: RatingState(-1), Rating: "G"}, Decision{false, ReasonRatingUnavailable}},
		{"undefined high state", restricted(Rules{AllowUnrated: true}), Classification{State: RatingState(3), Rating: "G"}, Decision{false, ReasonRatingUnavailable}},

		// Unrated.
		{"NR allow_unrated false", restricted(Rules{}), unrated, Decision{false, ReasonUnrated}},
		{"NR allow_unrated true", restricted(Rules{AllowUnrated: true}), unrated, Decision{true, ReasonAllowed}},
		{"NR allow_unrated true with ceiling", restricted(Rules{AllowUnrated: true, MaxRating: "G"}), unrated, Decision{true, ReasonAllowed}},
		{"unrated ignores rating text", restricted(Rules{}), Classification{State: Unrated, Rating: "G"}, Decision{false, ReasonUnrated}},

		// Unknown tokens on Rated are denied like unavailable (test 5).
		{"unknown token 15", restricted(Rules{AllowUnrated: true}), rated("15"), Decision{false, ReasonRatingUnavailable}},
		{"unknown token 12A", restricted(Rules{AllowUnrated: true, MaxRating: "X"}), rated("12A"), Decision{false, ReasonRatingUnavailable}},
		{"unknown token without ceiling", restricted(Rules{}), rated("15"), Decision{false, ReasonRatingUnavailable}},
		{"Rated with empty token", restricted(Rules{AllowUnrated: true}), rated(""), Decision{false, ReasonRatingUnavailable}},
		{"Rated with NR token", restricted(Rules{AllowUnrated: true}), rated("NR"), Decision{false, ReasonRatingUnavailable}},

		// Kids mode ceiling is PG; explicit max overrides in both directions.
		{"kids PG allowed", restricted(Rules{KidsMode: true}), rated("PG"), Decision{true, ReasonAllowed}},
		{"kids TV-PG allowed", restricted(Rules{KidsMode: true}), rated("TV-PG"), Decision{true, ReasonAllowed}},
		{"kids G allowed", restricted(Rules{KidsMode: true}), rated("G"), Decision{true, ReasonAllowed}},
		{"kids PG-13 denied", restricted(Rules{KidsMode: true}), rated("PG-13"), Decision{false, ReasonRatingAbove}},
		{"kids TV-14 denied", restricted(Rules{KidsMode: true}), rated("TV-14"), Decision{false, ReasonRatingAbove}},
		{"kids R denied", restricted(Rules{KidsMode: true}), rated("R"), Decision{false, ReasonRatingAbove}},
		{"kids NR denied by default", restricted(Rules{KidsMode: true}), unrated, Decision{false, ReasonUnrated}},
		{"kids NR with allow_unrated", restricted(Rules{KidsMode: true, AllowUnrated: true}), unrated, Decision{true, ReasonAllowed}},
		{"explicit max higher than PG allows PG-13", restricted(Rules{KidsMode: true, MaxRating: "PG-13"}), rated("PG-13"), Decision{true, ReasonAllowed}},
		{"explicit max higher still blocks R", restricted(Rules{KidsMode: true, MaxRating: "PG-13"}), rated("R"), Decision{false, ReasonRatingAbove}},
		{"explicit max lower than PG blocks PG", restricted(Rules{KidsMode: true, MaxRating: "G"}), rated("PG"), Decision{false, ReasonRatingAbove}},
		{"explicit max lower than PG allows G", restricted(Rules{KidsMode: true, MaxRating: "G"}), rated("G"), Decision{true, ReasonAllowed}},
		{"explicit ceil, no kids mode", restricted(Rules{MaxRating: "R"}), rated("TV-MA"), Decision{true, ReasonAllowed}},
		{"explicit ceil, no kids mode, above", restricted(Rules{MaxRating: "R"}), rated("NC-17"), Decision{false, ReasonRatingAbove}},
		{"max rating is case and space folded", restricted(Rules{MaxRating: " pg "}), rated("pg-13"), Decision{false, ReasonRatingAbove}},
		{"item rating is case folded", restricted(Rules{MaxRating: "PG"}), rated(" tv-g "), Decision{true, ReasonAllowed}},
		{"same level different system", restricted(Rules{MaxRating: "TV-14"}), rated("PG-13"), Decision{true, ReasonAllowed}},

		// Tags (ADR-0031 test 6).
		{"blocked tag denies", restricted(Rules{BlockedTags: []string{"gore"}}), rated("G", "gore"), Decision{false, ReasonBlockedTag}},
		{"blocked tag case and space", restricted(Rules{BlockedTags: []string{" Gore "}}), rated("G", "  GORE\t"), Decision{false, ReasonBlockedTag}},
		{"gore does not block gorey", restricted(Rules{BlockedTags: []string{"gore"}}), rated("G", "gorey"), Decision{true, ReasonAllowed}},
		{"gore does not block gore-free", restricted(Rules{BlockedTags: []string{"gore"}}), rated("G", "no-gore", "ogore"), Decision{true, ReasonAllowed}},
		{"blocked tag unrelated", restricted(Rules{BlockedTags: []string{"gore"}}), rated("G", "family"), Decision{true, ReasonAllowed}},
		{"blocked beats allowed", restricted(Rules{BlockedTags: []string{"gore"}, AllowedTags: []string{"family"}}), rated("G", "family", "gore"), Decision{false, ReasonBlockedTag}},
		{"allowed set with no match denied", restricted(Rules{AllowedTags: []string{"family"}}), rated("G", "action"), Decision{false, ReasonAllowedTagMissing}},
		{"allowed set with no tags denied", restricted(Rules{AllowedTags: []string{"family"}}), rated("G"), Decision{false, ReasonAllowedTagMissing}},
		{"allowed set with empty tag items denied", restricted(Rules{AllowedTags: []string{"family"}}), rated("G", "", "  "), Decision{false, ReasonAllowedTagMissing}},
		{"allowed set substring is no match", restricted(Rules{AllowedTags: []string{"family"}}), rated("G", "family-friendly"), Decision{false, ReasonAllowedTagMissing}},
		{"allowed match", restricted(Rules{AllowedTags: []string{"family"}}), rated("G", "Family"), Decision{true, ReasonAllowed}},
		{"any one allowed match is enough", restricted(Rules{AllowedTags: []string{"family", "kids"}}), rated("G", "kids"), Decision{true, ReasonAllowed}},
		{"allowed match cannot lift rating denial", restricted(Rules{MaxRating: "PG", AllowedTags: []string{"family"}}), rated("R", "family"), Decision{false, ReasonRatingAbove}},
		{"allowed match cannot lift kids denial", restricted(Rules{KidsMode: true, AllowedTags: []string{"family"}}), rated("PG-13", "family"), Decision{false, ReasonRatingAbove}},
		{"allowed match cannot lift unrated denial", restricted(Rules{AllowedTags: []string{"family"}}), Classification{State: Unrated, Tags: []string{"family"}, TagsKnown: true}, Decision{false, ReasonUnrated}},
		{"allowed match cannot lift unavailable denial", restricted(Rules{AllowUnrated: true, AllowedTags: []string{"family"}}), Classification{State: Unavailable, Tags: []string{"family"}, TagsKnown: true}, Decision{false, ReasonRatingUnavailable}},
		{"allowed match cannot lift unknown token", restricted(Rules{AllowedTags: []string{"family"}}), rated("15", "family"), Decision{false, ReasonRatingUnavailable}},
		{"unrated plus allowed match passes when allowed", restricted(Rules{AllowUnrated: true, AllowedTags: []string{"family"}}), Classification{State: Unrated, Tags: []string{"family"}, TagsKnown: true}, Decision{true, ReasonAllowed}},

		// Tag lookup failure with tag rules is denied (test 6).
		{"blocked rules, tags unknown", restricted(Rules{BlockedTags: []string{"gore"}}), Classification{State: Rated, Rating: "G"}, Decision{false, ReasonTagsUnknown}},
		{"allowed rules, tags unknown", restricted(Rules{AllowedTags: []string{"family"}}), Classification{State: Rated, Rating: "G", Tags: []string{"family"}}, Decision{false, ReasonTagsUnknown}},
		{"unrated ok but tags unknown", restricted(Rules{AllowUnrated: true, BlockedTags: []string{"gore"}}), Classification{State: Unrated}, Decision{false, ReasonTagsUnknown}},
		{"no tag rules, tags unknown is fine", restricted(Rules{MaxRating: "PG"}), Classification{State: Rated, Rating: "G"}, Decision{true, ReasonAllowed}},
		{"tags known and empty with blocked rules", restricted(Rules{BlockedTags: []string{"gore"}}), Classification{State: Rated, Rating: "G", TagsKnown: true}, Decision{true, ReasonAllowed}},
		{"rating denial reported before tags unknown", restricted(Rules{MaxRating: "G", BlockedTags: []string{"gore"}}), Classification{State: Rated, Rating: "R"}, Decision{false, ReasonRatingAbove}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.policy, tc.class)
			if got != tc.want {
				t.Fatalf("Evaluate = %+v, want %+v", got, tc.want)
			}
			if again := Evaluate(tc.policy, tc.class); again != got {
				t.Fatalf("not deterministic: %+v then %+v", got, again)
			}
		})
	}
}

func TestEvaluateZeroValuesDoNotPanicAndDeny(t *testing.T) {
	if got := Evaluate(Policy{}, Classification{}); got.Allowed || got.Reason != ReasonInvalidPolicy {
		t.Fatalf("zero/zero: %+v", got)
	}
	if got := Evaluate(restricted(Rules{}), Classification{}); got.Allowed {
		t.Fatalf("restricted/zero: %+v", got)
	}
	if got := Evaluate(Policy{Mode: "restricted"}, Classification{State: Rated, Rating: "G"}); got.Allowed {
		t.Fatalf("nil rules: %+v", got)
	}
	var zero Decision
	if zero.Allowed {
		t.Fatal("zero Decision must deny")
	}
}

func TestEvaluateDoesNotMutateInputs(t *testing.T) {
	rules := Rules{BlockedTags: []string{" Gore ", "HORROR"}, AllowedTags: []string{"Family"}, MaxRating: " pg "}
	policy := restricted(rules)
	tags := []string{" Family ", "GORE"}
	class := Classification{State: Rated, Rating: " g ", Tags: tags, TagsKnown: true}
	Evaluate(policy, class)
	if !slices.Equal(policy.Rules.BlockedTags, []string{" Gore ", "HORROR"}) || policy.Rules.MaxRating != " pg " || !slices.Equal(tags, []string{" Family ", "GORE"}) || class.Rating != " g " {
		t.Fatalf("inputs mutated: %+v %v %+v", policy.Rules, tags, class)
	}
}

// A restricted policy must never allow an item whose rating is unavailable,
// whatever the other rules or classification fields say.
func TestUnavailableRatingNeverAllowedUnderRestrictedPolicy(t *testing.T) {
	ratings := []string{"", "G", "PG", "R", "X", "NR", "UR", "15", "12A", "\x00", "unknown"}
	tagSets := [][]string{nil, {}, {"family"}, {"gore"}, {"family", "gore"}, {"", " "}}
	// Zero (Unavailable) plus values outside the defined states.
	states := []RatingState{Unavailable, RatingState(-1), RatingState(3), RatingState(100)}
	ceils := []string{"", "G", "PG", "PG-13", "R", "X"}
	blocked := [][]string{nil, {"gore"}, {"family"}}
	allowed := [][]string{nil, {"family"}, {"gore"}}
	n := 0
	for _, kids := range []bool{false, true} {
		for _, unrated := range []bool{false, true} {
			for _, ceil := range ceils {
				for _, b := range blocked {
					for _, a := range allowed {
						policy := restricted(Rules{KidsMode: kids, AllowUnrated: unrated, MaxRating: ceil, BlockedTags: b, AllowedTags: a})
						for _, st := range states {
							for _, r := range ratings {
								for _, tags := range tagSets {
									for _, known := range []bool{false, true} {
										n++
										got := Evaluate(policy, Classification{State: st, Rating: r, Tags: tags, TagsKnown: known})
										if got.Allowed || got.Reason != ReasonRatingUnavailable {
											t.Fatalf("policy %+v class {%d %q %v %v} => %+v", *policy.Rules, st, r, tags, known, got)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if n < 1000 {
		t.Fatalf("property sweep too small: %d", n)
	}
}

// Rating and unrated denials hold whatever tags are present: an allowed-tag
// match can only ever narrow, never widen.
func TestTagsNeverWidenRatingOrUnratedDenial(t *testing.T) {
	tags := []string{"family", "kids", "gore", ""}
	for _, c := range []Classification{
		{State: Rated, Rating: "R"},
		{State: Rated, Rating: "15"},
		{State: Unrated},
	} {
		c.Tags, c.TagsKnown = tags, true
		policy := restricted(Rules{MaxRating: "PG", AllowedTags: []string{"family", "kids"}})
		base := c
		base.Tags, base.TagsKnown = nil, false
		if Evaluate(policy, base).Allowed {
			t.Fatalf("baseline allowed: %+v", c)
		}
		if got := Evaluate(policy, c); got.Allowed {
			t.Fatalf("allowed tags lifted a denial: %+v => %+v", c, got)
		}
	}
}
