package parental

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const restrictedJSON = `{"version":1,"mode":"restricted","rules":{"kids_mode":true,"max_rating":" pg-13 ","blocked_tags":[" Horror ","HORROR","Gore"],"allowed_tags":["Family"],"allow_unrated":false}}`

func TestPolicyCanonicalContract(t *testing.T) {
	update, err := DecodeUpdate([]byte(`{"expected_revision":0,"policy":` + restrictedJSON + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if update.ExpectedRevision != 0 || update.Policy.Rules.MaxRating != "PG-13" || !update.Policy.Rules.KidsMode || update.Policy.Rules.AllowUnrated {
		t.Fatalf("unexpected policy: %+v", update)
	}
	if !reflect.DeepEqual(update.Policy.Rules.BlockedTags, []string{"gore", "horror"}) || !reflect.DeepEqual(update.Policy.Rules.AllowedTags, []string{"family"}) {
		t.Fatalf("tags: %+v", update.Policy.Rules)
	}
	raw, err := json.Marshal(update.Policy)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePolicy(raw)
	if err != nil || !reflect.DeepEqual(decoded, update.Policy) {
		t.Fatalf("canonical round trip: %+v %v", decoded, err)
	}
}

func TestAbsentAndUnrestrictedRemainDistinct(t *testing.T) {
	absent := Unconfigured(Scope{UserID: "kid", TenantID: "home"})
	if absent.State != "unconfigured" || absent.Revision != 0 || absent.Policy != nil {
		t.Fatalf("absence: %+v", absent)
	}
	unrestricted, err := DecodePolicy([]byte(`{"version":1,"mode":"unrestricted","rules":null}`))
	if err != nil || unrestricted.Mode != "unrestricted" || unrestricted.Rules != nil {
		t.Fatalf("unrestricted: %+v %v", unrestricted, err)
	}
	if _, err := DecodePolicy([]byte(`null`)); err == nil {
		t.Fatal("null policy became unrestricted")
	}
}

func TestPolicyRejectsAmbiguousOrSecretFields(t *testing.T) {
	validUpdate := `{"expected_revision":0,"policy":` + restrictedJSON + `}`
	cases := map[string]string{
		"missing revision":      `{"policy":` + restrictedJSON + `}`,
		"null revision":         strings.Replace(validUpdate, `"expected_revision":0`, `"expected_revision":null`, 1),
		"negative revision":     strings.Replace(validUpdate, `"expected_revision":0`, `"expected_revision":-1`, 1),
		"overflow revision":     strings.Replace(validUpdate, `"expected_revision":0`, `"expected_revision":9223372036854775807`, 1),
		"duplicate revision":    strings.Replace(validUpdate, `"expected_revision":0`, `"expected_revision":1,"expected_revision":0`, 1),
		"duplicate policy mode": strings.Replace(validUpdate, `"mode":"restricted"`, `"mode":"unrestricted","mode":"restricted"`, 1),
		"duplicate rule":        strings.Replace(validUpdate, `"allow_unrated":false`, `"allow_unrated":true,"allow_unrated":false`, 1),
		"unsupported version":   strings.Replace(validUpdate, `"version":1`, `"version":2`, 1),
		"null version":          strings.Replace(validUpdate, `"version":1`, `"version":null`, 1),
		"unknown rating":        strings.Replace(validUpdate, `" pg-13 "`, `"BANANAS"`, 1),
		"unrated ceiling":       strings.Replace(validUpdate, `" pg-13 "`, `"NR"`, 1),
		"camel case alias":      strings.Replace(validUpdate, `"kids_mode"`, `"kidsMode"`, 1),
		"null flag":             strings.Replace(validUpdate, `"kids_mode":true`, `"kids_mode":null`, 1),
		"string flag":           strings.Replace(validUpdate, `"kids_mode":true`, `"kids_mode":"true"`, 1),
		"missing flag":          strings.Replace(validUpdate, `"kids_mode":true,`, ``, 1),
		"null tags":             strings.Replace(validUpdate, `["Family"]`, `null`, 1),
		"null tag":              strings.Replace(validUpdate, `["Family"]`, `[null]`, 1),
		"empty tag":             strings.Replace(validUpdate, `["Family"]`, `["  "]`, 1),
		"control tag":           strings.Replace(validUpdate, `["Family"]`, `["a\nb"]`, 1),
		"PIN":                   strings.Replace(validUpdate, `"kids_mode":true`, `"pin":"1234","kids_mode":true`, 1),
		"PIN hash":              strings.Replace(validUpdate, `"version":1`, `"pin_hash":"secret","version":1`, 1),
		"tenant input":          strings.Replace(validUpdate, `"expected_revision":0`, `"tenant_id":"other","expected_revision":0`, 1),
		"trailing document":     validUpdate + `{}`,
		"not object":            `[]`,
		"null rules restricted": `{"expected_revision":0,"policy":{"version":1,"mode":"restricted","rules":null}}`,
		"rules on unrestricted": strings.Replace(validUpdate, `"mode":"restricted"`, `"mode":"unrestricted"`, 1),
		"missing rules":         `{"expected_revision":0,"policy":{"version":1,"mode":"unrestricted"}}`,
		"oversized input":       validUpdate + strings.Repeat(" ", MaxBodyBytes),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeUpdate([]byte(raw)); err == nil {
				t.Fatalf("accepted malformed policy: %s", raw)
			}
		})
	}
}

func TestPolicyTagBoundsAndScope(t *testing.T) {
	for _, tags := range [][]string{make([]string, 65), {strings.Repeat("x", 129)}} {
		if _, err := Normalize(Policy{Version: 1, Mode: "restricted", Rules: &Rules{BlockedTags: tags}}); err == nil {
			t.Fatal("accepted excessive tags")
		}
	}
	for _, scope := range []Scope{{}, {UserID: " kid"}, {UserID: "kid", TenantID: "\x00"}, {UserID: strings.Repeat("u", 257)}} {
		if ValidateScope(scope) == nil {
			t.Fatalf("accepted invalid identity: %+v", scope)
		}
	}
	if err := ValidateScope(Scope{UserID: "alice@example.com"}); err != nil {
		t.Fatalf("empty household tenant: %v", err)
	}
}
