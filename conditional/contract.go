// Package conditional defines the additive household HTTP contract in ADR-0038.
// Request IDs correlate responses only; this API supplies no durable receipts,
// deduplication, writer enforcement or progress/watched mutation operation.
package conditional

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/Muxcore-Media/userdata-local/internal/models"
	"github.com/Muxcore-Media/userdata-local/parental"
)

const (
	Capability       = "userdata.conditional.v2"
	Path             = "/api/userdata/v2"
	MaxRequestBytes  = 4 << 20
	MaxBlobBytes     = models.MaxBlobBytes
	MaxResponseBytes = MaxBlobBytes + 16<<10
)

// Error codes are also errors.Is targets. Only StorageUnavailable denotes an
// ambiguous application outcome. A refusal never resolves an earlier dispatch.
type Error string

func (e Error) Error() string { return string(e) }

const (
	ProtocolUnsupported  Error = "userdata.protocol_unsupported"
	MutationUnsupported  Error = "userdata.mutation_unsupported"
	InvalidBody          Error = "userdata.invalid_body"
	InvalidPrecondition  Error = "userdata.invalid_precondition"
	PreconditionRequired Error = "userdata.precondition_required"
	InvalidQuery         Error = "userdata.invalid_query"
	Unauthenticated      Error = "userdata.unauthenticated"
	Forbidden            Error = "userdata.forbidden"
	ScopeUnsupported     Error = "userdata.scope_unsupported"
	MethodNotAllowed     Error = "userdata.method_not_allowed"
	AuthorityConflict    Error = "userdata.authority_conflict"
	RevisionConflict     Error = "userdata.revision_conflict"
	TooLarge             Error = "userdata.too_large"
	IdentityUnavailable  Error = "userdata.identity_unavailable"
	StorageUnavailable   Error = "userdata.storage_unavailable"
	RevisionExhausted    Error = "userdata.revision_exhausted"
)

// Operations returns an independent, ordered list; manifest support alone does
// not establish that a caller's mutation family is available.
func Operations() []string {
	return []string{"prefs", "favorites", "wantToWatch", "playlists", "queue"}
}

type Precondition struct {
	Authority string `json:"authority"`
	UserID    string `json:"user_id"`
	TenantID  string `json:"tenant_id"`
	Revision  string `json:"revision"`
}

type Update struct {
	Contract  string          `json:"contract"`
	RequestID string          `json:"request_id"`
	Expected  Precondition    `json:"expected"`
	Mutation  json.RawMessage `json:"mutation"`
}

// Document is a raw read envelope. The last three members exist only in an ACK.
type Document struct {
	Contract         string          `json:"contract"`
	Authority        string          `json:"authority"`
	UserID           string          `json:"user_id"`
	TenantID         string          `json:"tenant_id"`
	Revision         string          `json:"revision"`
	Exists           bool            `json:"exists"`
	Operations       []string        `json:"operations"`
	LegacyWrites     bool            `json:"legacy_writes"`
	Blob             json.RawMessage `json:"blob"`
	Outcome          string          `json:"outcome,omitempty"`
	RequestID        string          `json:"request_id,omitempty"`
	PreviousRevision string          `json:"previous_revision,omitempty"`
}

func ValidID(id string) bool {
	return utf8.ValidString(id) && parental.ValidateScope(parental.Scope{UserID: id}) == nil
}

func ValidAuthority(authority string) bool {
	if len(authority) != 43 {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(authority)
	return err == nil && len(raw) == 32
}

func ValidRequestID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// ParseRevision never accepts a JSON number or a non-canonical decimal token.
func ParseRevision(revision string) (int64, error) {
	if revision == "" || (len(revision) > 1 && revision[0] == '0') {
		return 0, InvalidPrecondition
	}
	for _, c := range revision {
		if c < '0' || c > '9' {
			return 0, InvalidPrecondition
		}
	}
	n, err := strconv.ParseInt(revision, 10, 64)
	if err != nil {
		return 0, InvalidPrecondition
	}
	return n, nil
}

func (p Precondition) Validate() error {
	if !ValidAuthority(p.Authority) || !ValidID(p.UserID) || p.TenantID != "" {
		return InvalidPrecondition
	}
	_, err := ParseRevision(p.Revision)
	return err
}

func DecodeUpdate(raw []byte) (Update, error) {
	var update Update
	if len(raw) > MaxRequestBytes {
		return update, TooLarge
	}
	if ValidateJSON(raw) != nil {
		return update, InvalidBody
	}
	fields, err := object(raw)
	if err != nil {
		return update, InvalidBody
	}
	contract, ok := stringField(fields, "contract")
	if !ok {
		return update, InvalidBody
	}
	if contract != Capability {
		return update, ProtocolUnsupported
	}
	expected, exists := fields["expected"]
	if !exists {
		return update, PreconditionRequired
	}
	pre, err := object(expected)
	if err != nil || !exactFields(pre, []string{"authority", "user_id", "tenant_id", "revision"}) {
		return update, InvalidPrecondition
	}
	for _, key := range []string{"authority", "user_id", "tenant_id", "revision"} {
		if _, ok := stringField(pre, key); !ok {
			return update, InvalidPrecondition
		}
	}
	if !exactFields(fields, []string{"contract", "request_id", "expected", "mutation"}) || json.Unmarshal(raw, &update) != nil {
		return Update{}, InvalidBody
	}
	if err := ValidateUpdate(update); err != nil {
		return Update{}, err
	}
	return update, nil
}

// ValidateUpdate checks an already decoded value without re-encoding identity
// metadata. Wire callers must use DecodeUpdate for original byte limits and
// required member presence; re-encoding can expand valid Unicode metadata.
func ValidateUpdate(update Update) error {
	if update.Contract != Capability {
		return ProtocolUnsupported
	}
	if err := update.Expected.Validate(); err != nil {
		return err
	}
	if !ValidRequestID(update.RequestID) || ValidateJSON(update.Mutation) != nil {
		return InvalidBody
	}
	if len(update.Mutation) > MaxRequestBytes {
		return TooLarge
	}
	_, err := decodeMutation(update.Mutation)
	return err
}

func (d Document) Precondition() Precondition {
	return Precondition{Authority: d.Authority, UserID: d.UserID, TenantID: d.TenantID, Revision: d.Revision}
}

// EncodeDocument checks the actual wire representation, including all raw
// nested values. Call before a conditional write, never after committing.
func EncodeDocument(document Document) ([]byte, error) {
	raw, err := Encode(document)
	if err != nil {
		return nil, StorageUnavailable
	}
	if len(document.Blob) > MaxBlobBytes || len(raw) > MaxResponseBytes {
		return nil, TooLarge
	}
	if ValidateJSON(raw) != nil {
		return nil, StorageUnavailable
	}
	return raw, nil
}

// DecodeDocument accepts reads only. DecodeAcknowledgment additionally binds a
// successful PUT reply to the caller's immutable original dispatched update.
func DecodeDocument(raw []byte) (Document, error) {
	return decodeDocument(raw, nil)
}

func DecodeAcknowledgment(raw []byte, update Update) (Document, error) {
	return decodeDocument(raw, &update)
}

func decodeDocument(raw []byte, update *Update) (Document, error) {
	var document Document
	if len(raw) > MaxResponseBytes || ValidateJSON(raw) != nil {
		return document, errors.New("invalid conditional response")
	}
	fields, err := object(raw)
	required := []string{"contract", "authority", "user_id", "tenant_id", "revision", "exists", "operations", "legacy_writes", "blob"}
	if update != nil {
		required = append(required, "outcome", "request_id", "previous_revision")
	}
	if err != nil || !exactFields(fields, required) || json.Unmarshal(raw, &document) != nil {
		return Document{}, errors.New("invalid conditional response")
	}
	for _, key := range []string{"contract", "authority", "user_id", "tenant_id", "revision"} {
		if _, ok := stringField(fields, key); !ok {
			return Document{}, errors.New("invalid conditional response")
		}
	}
	revision, revErr := ParseRevision(document.Revision)
	_, blobErr := object(document.Blob)
	if document.Contract != Capability || document.Precondition().Validate() != nil ||
		revErr != nil || document.Exists != (revision > 0) || !document.LegacyWrites ||
		!slices.Equal(document.Operations, Operations()) || blobErr != nil || len(document.Blob) > MaxBlobBytes ||
		(!bytes.Equal(fields["exists"], []byte("true")) && !bytes.Equal(fields["exists"], []byte("false"))) {
		return Document{}, errors.New("invalid conditional response")
	}
	if update != nil {
		previous, err := ParseRevision(update.Expected.Revision)
		if update.Contract != Capability || update.Expected.Validate() != nil || !ValidRequestID(update.RequestID) ||
			err != nil || previous == math.MaxInt64 || revision != previous+1 ||
			document.Outcome != "applied" || document.RequestID != update.RequestID ||
			document.PreviousRevision != update.Expected.Revision || document.Authority != update.Expected.Authority ||
			document.UserID != update.Expected.UserID || document.TenantID != update.Expected.TenantID {
			return Document{}, errors.New("mismatched conditional acknowledgment")
		}
	}
	return document, nil
}
