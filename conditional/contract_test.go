package conditional

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const epoch = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func request(patch string) []byte {
	return []byte(`{"contract":"userdata.conditional.v2","request_id":"fixture-1","expected":{"authority":"` + epoch + `","user_id":"alice","tenant_id":"","revision":"0"},"mutation":` + patch + `}`)
}

func TestRequestRejectsAmbiguousAndUnsupportedIntent(t *testing.T) {
	valid := string(request(`{"queue":[]}`))
	tests := []struct {
		name, body string
		want       error
	}{
		{"protocol", strings.Replace(valid, Capability, "userdata.conditional.v3", 1), ProtocolUnsupported},
		{"missing precondition", `{"contract":"userdata.conditional.v2","request_id":"r","mutation":{"queue":[]}}`, PreconditionRequired},
		{"missing tenant", strings.Replace(valid, `"tenant_id":"",`, "", 1), InvalidPrecondition},
		{"number revision", strings.Replace(valid, `"revision":"0"`, `"revision":0`, 1), InvalidPrecondition},
		{"leading zero", strings.Replace(valid, `"revision":"0"`, `"revision":"00"`, 1), InvalidPrecondition},
		{"overflow", strings.Replace(valid, `"revision":"0"`, `"revision":"9223372036854775808"`, 1), InvalidPrecondition},
		{"epoch padding bits", strings.Replace(valid, epoch, strings.Repeat("A", 42)+"B", 1), InvalidPrecondition},
		{"tenant", strings.Replace(valid, `"tenant_id":""`, `"tenant_id":"a"`, 1), InvalidPrecondition},
		{"null request", strings.Replace(valid, `"request_id":"fixture-1"`, `"request_id":null`, 1), InvalidBody},
		{"long request", strings.Replace(valid, "fixture-1", strings.Repeat("a", 129), 1), InvalidBody},
		{"escaped duplicate", strings.Replace(valid, `"request_id":"fixture-1"`, `"request_id":"fixture-1","request_\u0069d":"other"`, 1), InvalidBody},
		{"nested duplicate", string(request(`{"queue":[{"future":{"a":1,"\u0061":2}}]}`)), InvalidBody},
		{"high surrogate", string(request(`{"prefs":{"future":"\ud800"}}`)), InvalidBody},
		{"low surrogate", string(request(`{"prefs":{"future":"\udfff"}}`)), InvalidBody},
		{"wrong pair", string(request(`{"prefs":{"future":"\ud800\u0041"}}`)), InvalidBody},
		{"UTF8", string(request(`{"prefs":{"future":"` + string([]byte{0xff}) + `"}}`)), InvalidBody},
		{"trailing", valid + "{}", InvalidBody},
		{"unknown envelope", strings.Replace(valid, `"request_id"`, `"other":true,"request_id"`, 1), InvalidBody},
		{"mixed progress", string(request(`{"prefs":{"display":{"theme":"light"}},"progress":{}}`)), MutationUnsupported},
		{"mixed watched", string(request(`{"queue":[],"watched":true}`)), MutationUnsupported},
		{"empty intent", string(request(`{"prefs":{},"favorites":{"set":{},"remove":[]}}`)), InvalidBody},
		{"known type", string(request(`{"prefs":{"display":[]}}`)), InvalidBody},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeUpdate([]byte(tc.body)); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
	for _, raw := range [][]byte{
		request(`{"prefs":{"future":"\ud83d\ude00"}}`),
		request(`{"prefs":{"literal":"\\ud800"}}`),
		request(`{"prefs":{},"queue":[]}`),
		[]byte(strings.Replace(valid, `"revision":"0"`, `"revision":"9223372036854775807"`, 1)),
	} {
		if _, err := DecodeUpdate(raw); err != nil {
			t.Fatalf("valid: %s: %v", raw, err)
		}
	}
}

func TestDepthAndRequestSizeBoundaries(t *testing.T) {
	if ValidateJSON([]byte(strings.Repeat("[", 64)+"0"+strings.Repeat("]", 64))) != nil {
		t.Fatal("64 containers rejected")
	}
	if ValidateJSON([]byte(strings.Repeat("[", 65)+"0"+strings.Repeat("]", 65))) == nil {
		t.Fatal("65 accepted")
	}
	base := request(`{"prefs":{"future":""}}`)
	body := bytes.Replace(base, []byte(`"future":""`), []byte(`"future":"`+strings.Repeat("x", MaxRequestBytes-len(base))+`"`), 1)
	if len(body) != MaxRequestBytes {
		t.Fatal("bad fixture")
	}
	if _, err := DecodeUpdate(body); err != nil {
		t.Fatalf("exact limit: %v", err)
	}
	if _, err := DecodeUpdate(append(body, ' ')); !errors.Is(err, TooLarge) {
		t.Fatalf("over limit: %v", err)
	}
}

func TestOriginalWireLimitDoesNotReencodeUnicodeMetadata(t *testing.T) {
	base := bytes.Replace(request(`{"prefs":{"future":""}}`), []byte("alice"), []byte("a\u2028b\u2029c"), 1)
	body := bytes.Replace(base, []byte(`"future":""`), []byte(`"future":"`+strings.Repeat("x", MaxRequestBytes-len(base))+`"`), 1)
	update, err := DecodeUpdate(body)
	if err != nil || len(body) != MaxRequestBytes {
		t.Fatalf("original wire body: %d %v", len(body), err)
	}
	encoded, err := Encode(update)
	if err != nil || len(encoded) <= MaxRequestBytes {
		t.Fatal("fixture did not exercise metadata expansion")
	}
	if err := ValidateUpdate(update); err != nil {
		t.Fatalf("decoded valid request failed provider validation: %v", err)
	}
}

func TestReadCannotAcknowledgeAndAckMustMatchOriginal(t *testing.T) {
	update, err := DecodeUpdate(request(`{"queue":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{Contract: Capability, Authority: epoch, UserID: "alice", Revision: "1", Exists: true, Operations: Operations(), LegacyWrites: true, Blob: json.RawMessage(`{"future":9007199254740993}`)}
	read, _ := Encode(doc)
	if _, err := DecodeDocument(read); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAcknowledgment(read, update); err == nil {
		t.Fatal("GET acknowledged")
	}
	doc.Outcome, doc.RequestID, doc.PreviousRevision = "applied", update.RequestID, "0"
	ack, _ := Encode(doc)
	if _, err := DecodeAcknowledgment(ack, update); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeDocument(ack); err == nil {
		t.Fatal("ACK accepted as GET")
	}
	for name, alter := range map[string]func(*Document){
		"request":    func(d *Document) { d.RequestID = "other" },
		"user":       func(d *Document) { d.UserID = "bob" },
		"tenant":     func(d *Document) { d.TenantID = "other" },
		"authority":  func(d *Document) { d.Authority = strings.Repeat("B", 42) + "A" },
		"previous":   func(d *Document) { d.PreviousRevision = "1" },
		"increment":  func(d *Document) { d.Revision = "2" },
		"operations": func(d *Document) { d.Operations = append(Operations(), "progress") },
		"legacy":     func(d *Document) { d.LegacyWrites = false },
		"exists":     func(d *Document) { d.Exists = false },
	} {
		t.Run(name, func(t *testing.T) {
			changed := doc
			alter(&changed)
			raw, _ := Encode(changed)
			if _, err := DecodeAcknowledgment(raw, update); err == nil {
				t.Fatal("mismatched ACK accepted")
			}
		})
	}
}

func TestEncodedLimitKeepsHTMLUnicodeAndNumericLexemes(t *testing.T) {
	marker := "<>&\u2028\u2029"
	prefix := `{"future":9007199254740993,"exponent":1.234567890123456789e+100,"text":"`
	suffix := `"}`
	value := strings.Repeat(marker, (MaxBlobBytes-len(prefix)-len(suffix))/len(marker))
	value += strings.Repeat("x", MaxBlobBytes-len(prefix)-len(suffix)-len(value))
	blob := json.RawMessage(prefix + value + suffix)
	if _, err := Apply(blob, json.RawMessage(`{"favorites":{"remove":["absent"]}}`)); !errors.Is(err, TooLarge) {
		t.Fatalf("oversize merge: %v", err)
	}
	doc := Document{Contract: Capability, Authority: epoch, UserID: "alice", Revision: "1", Exists: true, Operations: Operations(), LegacyWrites: true, Blob: blob}
	raw, err := EncodeDocument(doc)
	if err != nil || len(raw) > MaxResponseBytes {
		t.Fatalf("encoded limit: %d %v", len(raw), err)
	}
	for _, token := range []string{"9007199254740993", "1.234567890123456789e+100", marker} {
		if !bytes.Contains(raw, []byte(token)) {
			t.Fatalf("transformed %q", token)
		}
	}
	if _, err := DecodeDocument(raw); err != nil {
		t.Fatalf("unreadable accepted blob: %v", err)
	}
}
