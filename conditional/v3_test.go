package conditional

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func requestV3(patch string) []byte {
	return bytes.Replace(request(patch), []byte(Capability), []byte(CapabilityV3), 1)
}

func TestV3StrictEnvelopeAndVersionIsolation(t *testing.T) {
	valid := string(requestV3(`{"progress":{"report":{"x":{"positionSec":0.1,"durationSec":0}}}}`))
	tests := []struct {
		body string
		want error
	}{
		{strings.Replace(valid, CapabilityV3, Capability, 1), ProtocolUnsupported},
		{`{"contract":"userdata.conditional.v3"}`, PreconditionRequired},
		{strings.Replace(valid, `"tenant_id":"",`, "", 1), InvalidPrecondition},
		{strings.Replace(valid, `"revision":"0"`, `"revision":0`, 1), InvalidPrecondition},
		{strings.Replace(valid, `"revision":"0"`, `"revision":"00"`, 1), InvalidPrecondition},
		{strings.Replace(valid, `"revision":"0"`, `"revision":"9223372036854775808"`, 1), InvalidPrecondition},
		{strings.Replace(valid, `"tenant_id":""`, `"tenant_id":"t"`, 1), InvalidPrecondition},
		{strings.Replace(valid, `"request_id":"fixture-1"`, `"request_id":null`, 1), InvalidBody},
		{strings.Replace(valid, `"request_id":"fixture-1"`, `"request_id":"fixture-1","request_\u0069d":"other"`, 1), InvalidBody},
		{strings.Replace(valid, `"positionSec":0.1`, `"positionSec":0.1,"position\u0053ec":1`, 1), InvalidBody},
		{strings.Replace(valid, `"durationSec":0`, `"durationSec":0,"metadata":{"x":"\ud800"}`, 1), InvalidBody},
		{valid + "{}", InvalidBody},
		{strings.Replace(valid, `"request_id"`, `"other":true,"request_id"`, 1), InvalidBody},
		{string(requestV3(`{"progress":{"set_watched":{"x":true}},"watched":true}`)), MutationUnsupported},
		{string(requestV3(`{"progress":{},"prefs":{}}`)), InvalidBody},
		{string(requestV3(`{"progress":{"set_watched":{"x":true}},"prefs":{"display":[]}}`)), InvalidBody},
		{string(requestV3(`{"progress":{"report":{"x":{"positionSec":0,"durationSec":0,"metadata":{"future":"` + string([]byte{0xff}) + `"}}}}}`)), InvalidBody},
		{string(requestV3(`{"progress":{"report":{"x":{"positionSec":0,"durationSec":0,"metadata":{"deep":` + strings.Repeat("[", 64) + `0` + strings.Repeat("]", 64) + `}}}}}`)), InvalidBody},
	}
	for _, tc := range tests {
		if _, err := DecodeUpdateV3([]byte(tc.body)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v want %v", tc.body, err, tc.want)
		}
	}
	update, err := DecodeUpdateV3([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateUpdate(update); !errors.Is(err, ProtocolUnsupported) {
		t.Fatal("v2 accepted v3")
	}
	if _, err := DecodeUpdate(requestV3(`{"queue":[]}`)); !errors.Is(err, ProtocolUnsupported) {
		t.Fatal("v2 wire decoder accepted v3")
	}
	if _, err := DecodeUpdate(request(`{"queue":[],"progress":{}}`)); !errors.Is(err, MutationUnsupported) {
		t.Fatal("v2 accepted progress")
	}
	if !reflect.DeepEqual(Operations(), []string{"prefs", "favorites", "wantToWatch", "playlists", "queue"}) {
		t.Fatal("v2 operation list changed")
	}
	changed := OperationsV3()
	changed[0] = "other"
	if OperationsV3()[0] != "prefs" {
		t.Fatal("mutable operation list")
	}
}

func TestV3AcknowledgmentMatchesImmutableDispatch(t *testing.T) {
	update, err := DecodeUpdateV3(requestV3(`{"progress":{"set_watched":{"x":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{Contract: CapabilityV3, Authority: epoch, UserID: "alice", Revision: "1", Exists: true, Operations: OperationsV3(), LegacyWrites: true, Blob: json.RawMessage(`{"future":9007199254740993}`)}
	read, _ := EncodeDocument(doc)
	if _, err := DecodeDocumentV3(read); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAcknowledgmentV3(read, update); err == nil {
		t.Fatal("GET acknowledged")
	}
	if _, err := DecodeDocument(read); err == nil {
		t.Fatal("v2 accepted v3 response")
	}
	doc.Outcome, doc.RequestID, doc.PreviousRevision = "applied", update.RequestID, "0"
	ack, _ := EncodeDocument(doc)
	if _, err := DecodeAcknowledgmentV3(ack, update); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeDocumentV3(ack); err == nil {
		t.Fatal("ACK accepted as GET")
	}
	for name, alter := range map[string]func(*Document){
		"contract": func(d *Document) { d.Contract = Capability }, "request": func(d *Document) { d.RequestID = "other" }, "authority": func(d *Document) { d.Authority = strings.Repeat("B", 42) + "A" }, "user": func(d *Document) { d.UserID = "bob" }, "tenant": func(d *Document) { d.TenantID = "t" }, "previous": func(d *Document) { d.PreviousRevision = "1" }, "increment": func(d *Document) { d.Revision = "2" }, "legacy": func(d *Document) { d.LegacyWrites = false }, "exists": func(d *Document) { d.Exists = false }, "operations": func(d *Document) { d.Operations = Operations() }, "reorder": func(d *Document) {
			d.Operations = []string{"progress", "prefs", "favorites", "wantToWatch", "playlists", "queue"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := doc
			alter(&bad)
			raw, _ := EncodeDocument(bad)
			if _, err := DecodeAcknowledgmentV3(raw, update); err == nil {
				t.Fatal("mismatched ACK accepted")
			}
		})
	}
	for _, raw := range [][]byte{bytes.Replace(ack, []byte(`"exists":true`), []byte(`"exists":null`), 1), bytes.Replace(ack, []byte(`"legacy_writes":true`), []byte(`"legacy_writes":null`), 1), append(ack, []byte("{}")...)} {
		if _, err := DecodeAcknowledgmentV3(raw, update); err == nil {
			t.Fatal("malformed ACK accepted")
		}
	}
	wrong := update
	wrong.Contract = Capability
	if _, err := DecodeAcknowledgmentV3(ack, wrong); err == nil {
		t.Fatal("cross-version dispatch accepted")
	}
}

func TestV3MixedFamiliesAndRequestLimit(t *testing.T) {
	patch := `{"prefs":{"display":{"theme":"light"},"future":1e9999},"favorites":{"set":{"f":{"id":"f","future":9007199254740993}}},"wantToWatch":{"remove":["old"]},"playlists":[],"queue":[],"progress":{"report":{"x":{"positionSec":0.1250,"durationSec":0,"metadata":{"future":1e9999}}},"set_watched":{"x":false},"remove":["old"]}}`
	update, err := DecodeUpdateV3(requestV3(patch))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ApplyV3(json.RawMessage(`{"untouched":1.234567890123456789}`), update.Mutation, progressReceipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{`"positionSec":0.1250`, `9007199254740993`, `1e9999`, `1.234567890123456789`} {
		if !bytes.Contains(raw, []byte(token)) {
			t.Fatalf("lost raw %s", token)
		}
	}
	base := requestV3(`{"progress":{"report":{"x":{"positionSec":0,"durationSec":0,"metadata":{"future":""}}}}}`)
	body := bytes.Replace(base, []byte(`"future":""`), []byte(`"future":"`+strings.Repeat("x", MaxRequestBytes-len(base))+`"`), 1)
	if len(body) != MaxRequestBytes {
		t.Fatal("bad fixture")
	}
	if _, err := DecodeUpdateV3(body); err != nil {
		t.Fatalf("exact request limit: %v", err)
	}
	if _, err := DecodeUpdateV3(append(body, ' ')); !errors.Is(err, TooLarge) {
		t.Fatal("oversize accepted")
	}
}
