package conditional

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var progressReceipt = time.Date(2026, 10, 12, 10, 11, 12, 123456789, time.FixedZone("fixture", 3600))

func progressApply(t *testing.T, stored, patch string) map[string]json.RawMessage {
	t.Helper()
	raw, err := ApplyV3(json.RawMessage(stored), json.RawMessage(patch), progressReceipt)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := object(raw)
	progress, err := object(doc["progress"])
	if err != nil {
		t.Fatal(err)
	}
	return progress
}

func TestProgressNumericMathematicalRangeAndRawLexemes(t *testing.T) {
	for _, token := range []string{"0", "-0", "-0.000e999999", "0e-999999", "0.1", "9007199254740993", "1.234567890123456789", "5e-324", "4e-324", "1.7976931348623157e308"} {
		t.Run("valid "+token, func(t *testing.T) {
			if !validProgressNumber(json.RawMessage(token)) {
				t.Fatal("number refused")
			}
			got := progressApply(t, `{}`, `{"progress":{"report":{"x":{"positionSec":`+token+`,"durationSec":0}}}}`)
			entry, _ := object(got["x"])
			if string(entry["positionSec"]) != token {
				t.Fatalf("lexeme changed: %s", entry["positionSec"])
			}
		})
	}
	for _, token := range []string{"-1", "-0.1", "-1e-9999", "1e-9999", "2e-324", "-5e-324", "1e309", "-1e309", `"0"`, "null", "true", "NaN", "Infinity", "+0", "01", "0x1", "1 2"} {
		t.Run("invalid "+token, func(t *testing.T) {
			if validProgressNumber(json.RawMessage(token)) {
				t.Fatal("invalid number accepted")
			}
		})
	}
}

func TestProgressObservedTimeGrammarAndNormalization(t *testing.T) {
	valid := map[string]string{
		"2026-10-12T10:00:00+01:00":      "2026-10-12T09:00:00.000000000Z",
		"2026-10-12T08:00:00-01:00":      "2026-10-12T09:00:00.000000000Z",
		"2026-10-12T09:00:00.1Z":         "2026-10-12T09:00:00.100000000Z",
		"2026-10-12T09:00:00.123456789Z": "2026-10-12T09:00:00.123456789Z",
		"0000-02-29T00:00:00Z":           "0000-02-29T00:00:00.000000000Z",
		"2000-02-29T00:00:00-00:00":      "2000-02-29T00:00:00.000000000Z",
		"9999-12-31T23:59:59Z":           "9999-12-31T23:59:59.000000000Z",
		"2026-10-12T23:59:59+23:59":      "2026-10-12T00:00:59.000000000Z",
	}
	for input, want := range valid {
		encoded, _ := Encode(input)
		got, ok := normalizeObservation(encoded)
		expected, _ := Encode(want)
		if !ok || !bytes.Equal(got, expected) {
			t.Errorf("%s: %s %v", input, got, ok)
		}
	}
	invalid := []string{"", "2026-10-12", "2026-10-12t09:00:00Z", "2026-10-12T09:00:00z", "2026-10-12 09:00:00Z", "2026-10-12T09:00:00,1Z", "2026-10-12T09:00:00.Z", "2026-10-12T09:00:00.1234567890Z", "2026-10-12T24:00:00Z", "2026-10-12T09:60:00Z", "2026-10-12T09:00:60Z", "2026-10-12T09:00:00+24:00", "2026-10-12T09:00:00+00:60", "1900-02-29T00:00:00Z", "2026-04-31T00:00:00Z", "0000-01-01T00:00:00+00:01", "9999-12-31T23:59:59-00:01", "2026-1-12T09:00:00Z", "2026-10-12T9:00:00Z", "２０２６-10-12T09:00:00Z"}
	for _, input := range invalid {
		encoded, _ := Encode(input)
		if _, ok := normalizeObservation(encoded); ok {
			t.Errorf("accepted %q", input)
		}
	}
	for _, raw := range []string{"null", "0", "true"} {
		if _, ok := normalizeObservation(json.RawMessage(raw)); ok {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestProgressReportsWatchedRemovalAndRawPreservation(t *testing.T) {
	stored := `{"future":1e9999,"progress":{"x":{"id":"x","positionSec":17.1250,"durationSec":90,"watched":true,"observedAt":"2026-10-12T10:00:00+01:00","updatedAt":false,"title":"Before","future":9007199254740993,"nested":{"a":1,"b":2}},"untouched":null,"remove":false}}`
	patch := `{"progress":{"report":{"x":{"positionSec":92.00,"durationSec":100,"metadata":{"title":"After","nested":{"c":3},"new":1e9999}},"new":{"positionSec":120,"durationSec":100}},"set_watched":{"x":false,"watched":true},"remove":["remove","absent"]}}`
	raw, err := ApplyV3(json.RawMessage(stored), json.RawMessage(patch), progressReceipt)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := object(raw)
	entries, _ := object(doc["progress"])
	x, _ := object(entries["x"])
	added, _ := object(entries["new"])
	watched, _ := object(entries["watched"])
	if string(doc["future"]) != "1e9999" || string(x["future"]) != "9007199254740993" || string(x["new"]) != "1e9999" || string(x["positionSec"]) != "92.00" || string(x["watched"]) != "false" || string(x["nested"]) != `{"c":3}` {
		t.Fatalf("raw/shallow overlay failed: %s", raw)
	}
	if _, exists := x["observedAt"]; exists {
		t.Fatal("report without observation retained old value")
	}
	if _, exists := added["watched"]; exists {
		t.Fatal("report inferred watched")
	}
	if string(added["positionSec"]) != "120" || string(watched["positionSec"]) != "0" || string(watched["durationSec"]) != "0" || string(watched["watched"]) != "true" {
		t.Fatal("new entry defaults/completion changed")
	}
	for _, entry := range []map[string]json.RawMessage{x, added, watched} {
		if string(entry["updatedAt"]) != `"2026-10-12T09:11:12.123456789Z"` {
			t.Fatal("receipt not shared canonical UTC")
		}
	}
	if string(entries["untouched"]) != "null" {
		t.Fatal("unrelated malformed entry changed")
	}
	if _, exists := entries["remove"]; exists {
		t.Fatal("remove failed")
	}
	// Watched-only commands preserve valid observation bytes and unknown members.
	result := progressApply(t, stored, `{"progress":{"set_watched":{"x":false}}}`)
	x, _ = object(result["x"])
	if string(x["observedAt"]) != `"2026-10-12T10:00:00+01:00"` || string(x["positionSec"]) != "17.1250" {
		t.Fatal("watched-only normalized observation/position")
	}
	result = progressApply(t, stored, `{"progress":{"report":{"x":{"positionSec":88,"durationSec":100,"observedAt":"1900-01-01T01:00:00+01:00"}}}}`)
	x, _ = object(result["x"])
	if string(x["positionSec"]) != "0" || string(x["observedAt"]) != `"1900-01-01T00:00:00.000000000Z"` {
		t.Fatal("final watched did not override report")
	}
	result = progressApply(t, stored, `{"progress":{"report":{"x":{"positionSec":0,"durationSec":100}},"set_watched":{"x":false}}}`)
	x, _ = object(result["x"])
	if string(x["positionSec"]) != "0" || string(x["watched"]) != "false" {
		t.Fatal("explicit rewind failed")
	}
}

func TestProgressStrictCommandsAndAliases(t *testing.T) {
	invalid := []string{
		`null`, `[]`, `{}`, `{"report":{}}`, `{"set_watched":{}}`, `{"remove":[]}`, `{"report":null}`, `{"set_watched":null}`, `{"remove":null}`, `{"Report":{}}`, `{"unknown":{}}`,
		`{"report":{"x":null}}`, `{"report":{"x":{"positionSec":0}}}`, `{"report":{"x":{"positionSec":0,"durationSec":0,"extra":0}}}`, `{"report":{"x":{"PositionSec":0,"durationSec":0}}}`, `{"report":{"x":{"positionSec":0,"durationSec":0,"metadata":null}}}`, `{"report":{"x":{"positionSec":0,"durationSec":0,"observedAt":null}}}`,
		`{"set_watched":{"x":null}}`, `{"set_watched":{"x":0}}`, `{"set_watched":{"x":"true"}}`, `{"set_watched":{" x":true}}`, `{"remove":["x","x"]}`, `{"remove":[null]}`, `{"remove":["x"],"set_watched":{"x":false}}`, `{"remove":["x"],"report":{"x":{"positionSec":0,"durationSec":0}}}`,
	}
	for _, command := range invalid {
		_, err := ApplyV3(json.RawMessage(`{}`), json.RawMessage(`{"progress":`+command+`}`), progressReceipt)
		if !errors.Is(err, InvalidBody) {
			t.Errorf("%s: %v", command, err)
		}
	}
	for _, field := range progressFields {
		for _, alias := range []string{field, strings.ToUpper(field)} {
			value := `"ok"`
			if field == "kind" && alias == field {
				continue
			}
			patch := `{"progress":{"report":{"x":{"positionSec":0,"durationSec":0,"metadata":{"` + alias + `":` + value + `}}}}}`
			_, err := ApplyV3(json.RawMessage(`{}`), json.RawMessage(patch), progressReceipt)
			reserved := field == "id" || field == "positionSec" || field == "durationSec" || field == "watched" || field == "updatedAt" || field == "observedAt"
			if (reserved || alias != field) && !errors.Is(err, InvalidBody) {
				t.Errorf("metadata alias %s: %v", alias, err)
			}
		}
	}
	for _, name := range []string{"poſitionSec", "Kind", "STREAM_URL"} {
		_, err := ApplyV3(json.RawMessage(`{}`), json.RawMessage(`{"progress":{"report":{"x":{"positionSec":0,"durationSec":0,"metadata":{"`+name+`":"v"}}}}}`), progressReceipt)
		if !errors.Is(err, InvalidBody) {
			t.Errorf("Unicode/case alias %s accepted", name)
		}
	}
	for _, value := range []string{"null", "1", "false", "{}", "[]"} {
		_, err := ApplyV3(json.RawMessage(`{}`), json.RawMessage(`{"progress":{"report":{"x":{"positionSec":0,"durationSec":0,"metadata":{"title":`+value+`}}}}}`), progressReceipt)
		if !errors.Is(err, InvalidBody) {
			t.Errorf("metadata wrong type %s", value)
		}
	}
}

func TestProgressStoredRefusalRepairAndEmptyIntent(t *testing.T) {
	for _, stored := range []string{`{"progress":null}`, `{"progress":[]}`, `{"progress":1}`} {
		for _, empty := range []string{`{}`, `{"report":{},"set_watched":{},"remove":[]}`} {
			got, err := ApplyV3(json.RawMessage(stored), json.RawMessage(`{"queue":[],"progress":`+empty+`}`), progressReceipt)
			if err != nil || !bytes.Contains(got, []byte(strings.TrimSuffix(strings.TrimPrefix(stored, "{"), "}"))) {
				t.Fatalf("empty intent read malformed map: %s %v", got, err)
			}
		}
		if _, err := ApplyV3(json.RawMessage(stored), json.RawMessage(`{"progress":{"remove":["x"]}}`), progressReceipt); !errors.Is(err, StorageUnavailable) {
			t.Fatal("malformed map accepted")
		}
	}
	for _, entry := range []string{`null`, `1`, `[]`, `{"id":null}`, `{"id":"other"}`, `{"ID":"x"}`, `{"poſitionSec":1}`, `{"kind":null}`, `{"durationSec":"bad"}`, `{"observedAt":"bad"}`} {
		stored := `{"progress":{"x":` + entry + `}}`
		if _, err := ApplyV3(json.RawMessage(stored), json.RawMessage(`{"progress":{"set_watched":{"x":true}}}`), progressReceipt); !errors.Is(err, StorageUnavailable) {
			t.Errorf("stored %s accepted: %v", entry, err)
		}
		if result := progressApply(t, stored, `{"progress":{"remove":["x"]}}`); len(result) != 0 {
			t.Fatal("remove validated entry")
		}
	}
	// Canonical replacements repair invalid values, but identity/aliases cannot be repaired.
	result := progressApply(t, `{"progress":{"x":{"positionSec":"bad","durationSec":"bad","watched":1,"observedAt":null,"updatedAt":[]}}}`, `{"progress":{"report":{"x":{"positionSec":12,"durationSec":100}},"set_watched":{"x":true}}}`)
	entry, _ := object(result["x"])
	if string(entry["id"]) != `"x"` || string(entry["positionSec"]) != "0" {
		t.Fatal("repair/default identity failed")
	}
	result = progressApply(t, `{"progress":{"x":{"positionSec":"bad"}}}`, `{"progress":{"set_watched":{"x":true}}}`)
	entry, _ = object(result["x"])
	if _, exists := entry["durationSec"]; exists {
		t.Fatal("existing missing duration initialized")
	}
	result = progressApply(t, `{}`, `{"progress":{"remove":["absent"]}}`)
	if len(result) != 0 {
		t.Fatal("absent removal did not create empty map")
	}
}

func TestProgressFinalSizeAfterMixedRemoval(t *testing.T) {
	stored, _ := Encode(map[string]any{"progress": map[string]any{"remove": strings.Repeat("x", MaxBlobBytes-100)}})
	patch, _ := Encode(map[string]any{"prefs": map[string]any{"future": strings.Repeat("y", 500)}, "progress": map[string]any{"remove": []string{"remove"}}})
	result, err := ApplyV3(stored, patch, progressReceipt)
	if err != nil || len(result) >= 1000 {
		t.Fatalf("intermediate size refused final small document: %d %v", len(result), err)
	}
	stored, _ = Encode(map[string]any{"future": strings.Repeat("x", MaxBlobBytes-100)})
	if _, err := ApplyV3(stored, json.RawMessage(`{"progress":{"report":{"x":{"positionSec":1,"durationSec":2,"metadata":{"title":"long enough to exceed the exact final blob limit"}}}}}`), progressReceipt); !errors.Is(err, TooLarge) {
		t.Fatalf("oversized final blob accepted: %v", err)
	}
}

func TestProgressNumericRefusalsThroughPublicAPIAndStoredValues(t *testing.T) {
	for _, field := range []string{"positionSec", "durationSec"} {
		for _, token := range []string{"-1", "-1e-9999", "1e-9999", "2e-324", "1e309", "null", `"0"`} {
			t.Run(field+"/"+token, func(t *testing.T) {
				patch := `{"progress":{"report":{"x":{"positionSec":0,"durationSec":0}}}}`
				patch = strings.Replace(patch, `"`+field+`":0`, `"`+field+`":`+token, 1)
				if _, err := DecodeUpdateV3(requestV3(patch)); !errors.Is(err, InvalidBody) {
					t.Fatalf("wire number accepted: %v", err)
				}
				if _, err := ApplyV3(json.RawMessage(`{}`), json.RawMessage(patch), progressReceipt); !errors.Is(err, InvalidBody) {
					t.Fatalf("direct command accepted: %v", err)
				}
				stored := json.RawMessage(`{"progress":{"x":{"` + field + `":` + token + `}}}`)
				if _, err := ApplyV3(stored, json.RawMessage(`{"progress":{"set_watched":{"x":false}}}`), progressReceipt); !errors.Is(err, StorageUnavailable) {
					t.Fatalf("retained number accepted: %v", err)
				}
			})
		}
	}
}

func TestProgressUnknownNearAliasesAndAbsentEmptyIntent(t *testing.T) {
	result, err := ApplyV3(json.RawMessage(`{"future":1e9999}`), json.RawMessage(`{"queue":[],"progress":{"report":{},"set_watched":{},"remove":[]}}`), progressReceipt)
	if err != nil || bytes.Contains(result, []byte(`"progress"`)) {
		t.Fatalf("empty intent initialized progress: %s %v", result, err)
	}
	entries := progressApply(t, `{}`, `{"progress":{"report":{"x":{"positionSec":0,"durationSec":0,"metadata":{"position_sec":1e9999,"kind ":null," Kind":9007199254740993,"posterUrl":false}}}}}`)
	entry, _ := object(entries["x"])
	for name, want := range map[string]string{"position_sec": "1e9999", "kind ": "null", " Kind": "9007199254740993", "posterUrl": "false"} {
		if string(entry[name]) != want {
			t.Fatalf("unknown near-name %q was treated as alias: %s", name, entry[name])
		}
	}
}

func TestProgressReceiptMayRegressWithoutSelectingWinner(t *testing.T) {
	first, err := ApplyV3(json.RawMessage(`{}`), json.RawMessage(`{"progress":{"report":{"x":{"positionSec":90,"durationSec":100,"observedAt":"2099-01-01T00:00:00Z"}}}}`), progressReceipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []time.Time{progressReceipt, progressReceipt.Add(-time.Hour)} {
		last, err := ApplyV3(first, json.RawMessage(`{"progress":{"report":{"x":{"positionSec":2,"durationSec":100,"observedAt":"1900-01-01T00:00:00Z"}}}}`), receipt)
		if err != nil || !bytes.Contains(last, []byte(`"positionSec":2,`)) || !bytes.Contains(last, []byte(receipt.UTC().Format(receiptLayout))) {
			t.Fatalf("receipt clock prevented replacement: %s %v", last, err)
		}
	}
}

func TestProgressFinalBlobExactLimitIncludesReceiptAndDefaults(t *testing.T) {
	patch := json.RawMessage(`{"progress":{"report":{"x":{"positionSec":1.250,"durationSec":0,"metadata":{"title":"<>&"}}}}}`)
	base, err := ApplyV3(json.RawMessage(`{"future":""}`), patch, progressReceipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []int{0, 1} {
		stored, _ := Encode(map[string]string{"future": strings.Repeat("x", MaxBlobBytes-len(base)+extra)})
		result, err := ApplyV3(stored, patch, progressReceipt)
		if extra == 0 && (err != nil || len(result) != MaxBlobBytes || !bytes.Contains(result, []byte(`"title":"<>&"`))) {
			t.Fatalf("exact final boundary refused/amplified: %d %v", len(result), err)
		}
		if extra == 1 && !errors.Is(err, TooLarge) {
			t.Fatalf("one byte oversized final boundary accepted: %v", err)
		}
	}
}
