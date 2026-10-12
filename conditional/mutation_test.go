package conditional

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFiveFamilyMutationPreservesRawState(t *testing.T) {
	before := json.RawMessage(`{"unknown":{"big":9007199254740993,"precise":1.234567890123456789e+99},"progress":{"movie":{"updatedAt":"legacy-invalid","future":9007199254740995}},"prefs":{"display":{"theme":"dark","libraryPageSize":96},"future":{"keep":1,"old":2},"parental":{"pin_hash":"untrusted"}},"favorites":{"keep":{"id":"keep","future":9},"remove":{"id":"remove"}},"wantToWatch":{"remove":{"id":"remove"}},"playlists":[{"id":"old"}],"queue":[{"id":"old"}]}`)
	patch := json.RawMessage(`{"prefs":{"display":{"theme":"light"},"future":{"old":null,"add":9007199254740997}},"favorites":{"set":{"new":{"id":"new","future":9007199254740999}},"remove":["remove"]},"wantToWatch":{"set":{"later":{"id":"later","future":[1,2]}},"remove":["remove"]},"playlists":[{"id":"new","name":"New","itemIds":["x"],"future":9007199254741001}],"queue":[]}`)
	next, err := Apply(before, patch)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{`"libraryPageSize":96`, `"theme":"light"`, `"old":null`, `"pin_hash":"untrusted"`, "9007199254740993", "9007199254740995", "9007199254740997", "9007199254740999", "9007199254741001", "1.234567890123456789e+99", `"queue":[]`} {
		if !bytes.Contains(next, []byte(token)) {
			t.Fatalf("lost %s: %s", token, next)
		}
	}
	var oldFields, newFields map[string]json.RawMessage
	_ = json.Unmarshal(before, &oldFields)
	_ = json.Unmarshal(next, &newFields)
	if !bytes.Equal(oldFields["progress"], newFields["progress"]) || !bytes.Equal(oldFields["unknown"], newFields["unknown"]) {
		t.Fatal("untouched raw values changed")
	}
	for _, family := range []string{"favorites", "wantToWatch"} {
		var entries map[string]json.RawMessage
		_ = json.Unmarshal(newFields[family], &entries)
		if _, ok := entries["remove"]; ok {
			t.Fatalf("%s removal ignored", family)
		}
	}
	if !bytes.Contains(before, []byte(`"theme":"dark"`)) {
		t.Fatal("input changed")
	}
}

func TestCollectionIdentifierBoundsPreserveNullAndOmission(t *testing.T) {
	for _, field := range []string{"queue id", "playlist id", "playlist item"} {
		for _, id := range []string{"", " a", "a ", "a\nb", strings.Repeat("a", 257)} {
			rawID, _ := Encode(id)
			patch := `{"queue":[{"id":` + string(rawID) + `}]}`
			switch field {
			case "playlist id":
				patch = `{"playlists":[{"id":` + string(rawID) + `}]}`
			case "playlist item":
				patch = `{"playlists":[{"itemIds":[` + string(rawID) + `]}]}`
			}
			if _, err := Apply(json.RawMessage(`{}`), json.RawMessage(patch)); !errors.Is(err, InvalidBody) {
				t.Fatalf("%s accepted invalid identifier %q: %v", field, id, err)
			}
		}
	}
	id := strings.Repeat("a", 256)
	patch := json.RawMessage(`{"queue":[null,{},{"id":null},{"id":"` + id + `"}],"playlists":[null,{},{"id":null,"itemIds":null},{"id":"` + id + `","itemIds":[null,"` + id + `"]}]}`)
	got, err := Apply(json.RawMessage(`{}`), patch)
	if err != nil {
		t.Fatalf("valid exact-boundary/null identifiers: %v", err)
	}
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(patch, &before)
	_ = json.Unmarshal(got, &after)
	for _, field := range []string{"queue", "playlists"} {
		if !bytes.Equal(before[field], after[field]) {
			t.Fatalf("%s null/omitted/raw values changed: %s", field, got)
		}
	}
}

func TestCollectionIdentifierAliasesCannotBypassBounds(t *testing.T) {
	for _, patch := range []string{
		`{"queue":[{"ID":" bad"}]}`,
		`{"queue":[{"id":"ok","Id":" bad"}]}`,
		`{"playlists":[{"ID":" bad","id":"ok"}]}`,
		`{"playlists":[{"ITEMIDS":[" bad"]}]}`,
		`{"playlists":[{"itemIds":[],"ItemIds":[" bad"]}]}`,
		`{"playlists":[{"itemIdſ":[" bad"]}]}`,
	} {
		if _, err := Apply(json.RawMessage(`{}`), json.RawMessage(patch)); !errors.Is(err, InvalidBody) {
			t.Fatalf("accepted aliased identifier %s: %v", patch, err)
		}
	}
	patch := json.RawMessage(`{"queue":[{"ID":null,"id":"ok"}],"playlists":[{"Id":null,"ITEMIDS":[null,"ok"]}]}`)
	got, err := Apply(json.RawMessage(`{}`), patch)
	if err != nil || !bytes.Contains(got, []byte(`"ID":null`)) || !bytes.Contains(got, []byte(`"ITEMIDS":[null,"ok"]`)) {
		t.Fatalf("valid alias/null representation changed: %s %v", got, err)
	}
}

func TestMapIdentifierAliasesRetainEntryIdentity(t *testing.T) {
	for _, family := range []string{"favorites", "wantToWatch"} {
		for _, entry := range []string{
			`{"id":"a","ID":"b"}`, `{"id":"a","Id":" bad"}`,
			`{"ID":"b","id":"a"}`, `{"ID":"a"}`, `{"id":"a","ID":7}`,
		} {
			patch := json.RawMessage(`{"` + family + `":{"set":{"a":` + entry + `}}}`)
			if _, err := Apply(json.RawMessage(`{}`), patch); !errors.Is(err, InvalidBody) {
				t.Fatalf("%s accepted contradictory identity %s: %v", family, entry, err)
			}
		}
		patch := json.RawMessage(`{"` + family + `":{"set":{"a":{"id":"a","ID":null,"Id":"a","future":9007199254740993}}}}`)
		got, err := Apply(json.RawMessage(`{}`), patch)
		if err != nil || !bytes.Contains(got, []byte(`"ID":null`)) || !bytes.Contains(got, []byte(`"future":9007199254740993`)) {
			t.Fatalf("%s lost compatible aliases/unknown values: %s %v", family, got, err)
		}
	}
}

func TestReplacementPresenceAndNoop(t *testing.T) {
	before := json.RawMessage(`{"favorites":{"a":{"id":"a","future":1}},"prefs":{"future":{"keep":1},"nullable":5}}`)
	next, err := Apply(before, json.RawMessage(`{"favorites":{"set":{"a":{"id":"a","title":"replacement"}}},"prefs":{"future":{},"nullable":null,"new":{"x":1}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(next, []byte(`"future":1`)) || !bytes.Contains(next, []byte(`"keep":1`)) || !bytes.Contains(next, []byte(`"nullable":null`)) {
		t.Fatalf("bad replacement boundary: %s", next)
	}
	empty, err := Apply(json.RawMessage(`{}`), json.RawMessage(`{"prefs":{},"favorites":{},"queue":[]}`))
	if err != nil || string(empty) != `{"queue":[]}` {
		t.Fatalf("empty operations added defaults: %s %v", empty, err)
	}
	for _, patch := range []string{`{"favorites":{"remove":["a"]}}`, `{"favorites":{"remove":["a"]}}`, `{"favorites":{"set":{"a":{"id":"a","title":"readded"}}}}`} {
		next, err = Apply(next, json.RawMessage(patch))
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Contains(next, []byte("readded")) {
		t.Fatal("re-add failed")
	}
	for _, family := range []string{"prefs", "favorites", "wantToWatch"} {
		stored := json.RawMessage(`{"` + family + `":null}`)
		patch := `{"` + family + `":{"remove":["a"]}}`
		if family == "prefs" {
			patch = `{"prefs":{"new":1}}`
		}
		if _, err := Apply(stored, json.RawMessage(patch)); !errors.Is(err, StorageUnavailable) {
			t.Fatalf("stored %s null overwritten: %v", family, err)
		}
	}
	next, err = Apply(json.RawMessage(`{"prefs":{"future":null},"queue":null}`), json.RawMessage(`{"prefs":{"future":{"x":1}},"queue":[]}`))
	if err != nil || !bytes.Contains(next, []byte(`"x":1`)) {
		t.Fatalf("replacement refused: %s %v", next, err)
	}
}

func TestRefusalsAreAtomic(t *testing.T) {
	for _, patch := range []string{
		`{}`, `null`, `[]`, `{"prefs":null}`, `{"favorites":null}`, `{"queue":null}`,
		`{"favorites":{"set":null},"queue":[]}`, `{"favorites":{"remove":null},"queue":[]}`,
		`{"favorites":{"toggle":"a"}}`, `{"favorites":{"set":{"a":null}}}`,
		`{"favorites":{"set":{"a":{"id":"b"}}}}`, `{"favorites":{"remove":["a","a"]}}`,
		`{"favorites":{"remove":["a"],"set":{"a":{"id":"a"}}}}`, `{"favorites":{"remove":[" a"]}}`,
		`{"wantToWatch":{"set":{"a":{"id":"a","id":"a"}}}}`,
		`{"prefs":{"display":{"libraryPageSize":"wrong"}}}`, `{"playlists":[1]}`, `{"queue":[],"progress":{}}`,
	} {
		before := json.RawMessage(`{"favorites":{"a":{"id":"a"}},"queue":[{"id":"x"}]}`)
		copyBefore := append([]byte(nil), before...)
		if result, err := Apply(before, json.RawMessage(patch)); err == nil || result != nil {
			t.Fatalf("accepted %s: %s %v", patch, result, err)
		}
		if !bytes.Equal(before, copyBefore) {
			t.Fatal("refusal changed original")
		}
	}
}
