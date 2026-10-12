package conditional

import (
	"encoding/json"
	"time"
)

const (
	CapabilityV3 = "userdata.conditional.v3"
	PathV3       = "/api/userdata/v3"
)

// OperationsV3 is distinct from the frozen five-operation v2 contract.
func OperationsV3() []string {
	return []string{"prefs", "favorites", "wantToWatch", "playlists", "queue", "progress"}
}

func DecodeUpdateV3(raw []byte) (Update, error) {
	return decodeUpdate(raw, CapabilityV3, ValidateUpdateV3)
}

func ValidateUpdateV3(update Update) error {
	if update.Contract != CapabilityV3 {
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
	_, _, err := decodeMutationV3(update.Mutation)
	return err
}

func DecodeDocumentV3(raw []byte) (Document, error) {
	return decodeDocument(raw, nil, CapabilityV3, OperationsV3())
}

func DecodeAcknowledgmentV3(raw []byte, update Update) (Document, error) {
	return decodeDocument(raw, &update, CapabilityV3, OperationsV3())
}

func decodeMutationV3(raw []byte) (mutation, progressOperation, error) {
	var progress progressOperation
	fields, err := object(raw)
	if err != nil {
		return mutation{}, progress, InvalidBody
	}
	// Unsupported top-level families retain their distinct refusal code.
	for family := range fields {
		switch family {
		case "prefs", "favorites", "wantToWatch", "playlists", "queue", "progress":
		default:
			return mutation{}, progress, MutationUnsupported
		}
	}
	if value, exists := fields["progress"]; exists {
		progress, err = decodeProgress(value)
		if err != nil {
			return mutation{}, progress, err
		}
		delete(fields, "progress")
	}
	base, err := Encode(fields)
	if err != nil {
		return mutation{}, progress, InvalidBody
	}
	decoded, err := decodeBaseMutation(base, !progress.intent())
	return decoded, progress, err
}

// ApplyV3 preserves raw intent and uses one caller-sampled provider receipt time.
// It never compares event clocks, infers watched state or changes its inputs.
// The Store must validate the precondition and persist the result atomically.
func ApplyV3(stored, patch json.RawMessage, receipt time.Time) (json.RawMessage, error) {
	if err := ValidateBlob(stored); err != nil {
		return nil, err
	}
	if ValidateJSON(patch) != nil {
		return nil, InvalidBody
	}
	base, progress, err := decodeMutationV3(patch)
	if err != nil {
		return nil, err
	}
	return applyMutation(stored, base, func(document map[string]json.RawMessage) error {
		if !progress.intent() {
			return nil
		}
		current, err := targetObject(document, "progress")
		if err != nil {
			return err
		}
		if err := applyProgress(current, progress, receipt); err != nil {
			return err
		}
		document["progress"], err = Encode(current)
		if err != nil {
			return StorageUnavailable
		}
		return nil
	})
}
