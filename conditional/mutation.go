package conditional

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Muxcore-Media/userdata-local/internal/models"
)

type mapOperation struct {
	set    map[string]json.RawMessage
	remove []string
}

type mutation struct {
	fields map[string]json.RawMessage
	maps   map[string]mapOperation
}

func decodeMutation(raw []byte) (mutation, error) {
	return decodeBaseMutation(raw, true)
}

func decodeBaseMutation(raw []byte, requireIntent bool) (mutation, error) {
	result := mutation{maps: map[string]mapOperation{}}
	fields, err := object(raw)
	if err != nil {
		return result, InvalidBody
	}
	result.fields = fields
	intent := false
	for family := range fields {
		switch family {
		case "prefs", "favorites", "wantToWatch", "playlists", "queue":
		default:
			return result, MutationUnsupported
		}
	}
	for family, value := range fields {
		switch family {
		case "prefs":
			prefs, err := object(value)
			if err != nil || !compatible(family, value) {
				return result, InvalidBody
			}
			intent = intent || len(prefs) > 0
		case "favorites", "wantToWatch":
			op, err := decodeMap(value)
			if err != nil {
				return result, err
			}
			if family == "favorites" {
				set, err := Encode(op.set)
				if err != nil || !compatible(family, set) {
					return result, InvalidBody
				}
			}
			result.maps[family] = op
			intent = intent || len(op.set) > 0 || len(op.remove) > 0
		case "playlists", "queue":
			if !array(value) || !compatible(family, value) || !collectionIDs(family, value) {
				return result, InvalidBody
			}
			intent = true
		default:
			return result, MutationUnsupported
		}
	}
	if requireIntent && !intent {
		return result, InvalidBody
	}
	return result, nil
}

// Null and omitted identifiers retain the legacy decoder's representation.
// Every identifier actually supplied as a string obeys the same bounded ID
// contract as map keys; no typed validation copy is serialized.
func collectionIDs(family string, raw json.RawMessage) bool {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return false
	}
	for _, entry := range entries {
		if isNull(entry) {
			continue
		}
		fields, err := object(entry)
		if err != nil {
			return false
		}
		if !entryIDs(family, fields) {
			return false
		}
	}
	return true
}

func entryIDs(family string, fields map[string]json.RawMessage) bool {
	for name, raw := range fields {
		if isNull(raw) {
			continue
		}
		// Match encoding/json's case-insensitive known-field recognition,
		// checking every alias even when the canonical field also appears.
		if strings.EqualFold(name, "id") {
			if !stringID(raw) {
				return false
			}
		} else if family == "playlists" && strings.EqualFold(name, "itemIds") {
			var ids []json.RawMessage
			if json.Unmarshal(raw, &ids) != nil {
				return false
			}
			for _, id := range ids {
				if !isNull(id) && !stringID(id) {
					return false
				}
			}
		}
	}
	return true
}

func stringID(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && ValidID(value)
}

func isNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeMap(raw []byte) (mapOperation, error) {
	op := mapOperation{}
	fields, err := object(raw)
	if err != nil || !exactFields(fields, nil, "set", "remove") {
		return op, InvalidBody
	}
	if value, exists := fields["set"]; exists {
		op.set, err = object(value)
		if err != nil {
			return op, InvalidBody
		}
		for id, entry := range op.set {
			entryFields, err := object(entry)
			gotID, ok := stringField(entryFields, "id")
			if !ValidID(id) || err != nil || !ok || id != gotID {
				return op, InvalidBody
			}
			for name, value := range entryFields {
				if strings.EqualFold(name, "id") && !isNull(value) {
					alias, ok := stringField(entryFields, name)
					if !ok || alias != id {
						return op, InvalidBody
					}
				}
			}
		}
	}
	if value, exists := fields["remove"]; exists {
		if !array(value) || json.Unmarshal(value, &op.remove) != nil {
			return op, InvalidBody
		}
		seen := map[string]bool{}
		for _, id := range op.remove {
			if !ValidID(id) || seen[id] {
				return op, InvalidBody
			}
			if _, conflict := op.set[id]; conflict {
				return op, InvalidBody
			}
			seen[id] = true
		}
	}
	return op, nil
}

// compatible checks only affected known types. It never serializes the typed
// copy; unknown fields and numeric lexemes stay in the original raw values.
func compatible(family string, raw json.RawMessage) bool {
	wrapped, err := Encode(map[string]json.RawMessage{family: raw})
	if err != nil {
		return false
	}
	var validation models.Blob
	return json.Unmarshal(wrapped, &validation) == nil
}

// InitialBlob constructs defaults only for an absent row. Existing documents
// are never normalized through it, including when individual fields are absent.
func InitialBlob() json.RawMessage {
	raw, _ := Encode(map[string]any{
		"prefs":    models.DefaultPreferences(),
		"progress": map[string]any{}, "favorites": map[string]any{}, "wantToWatch": map[string]any{},
		"playlists": []any{}, "queue": []any{},
	})
	return raw
}

// ValidateBlob checks the raw document boundary without applying typed defaults.
func ValidateBlob(raw []byte) error {
	if len(raw) > MaxBlobBytes || ValidateJSON(raw) != nil {
		return StorageUnavailable
	}
	if _, err := object(raw); err != nil {
		return StorageUnavailable
	}
	return nil
}

// Apply builds an independent raw document; it never changes the input. Callers
// must still compare authority/revision and persist atomically.
func Apply(stored, patch json.RawMessage) (json.RawMessage, error) {
	if err := ValidateBlob(stored); err != nil {
		return nil, err
	}
	if ValidateJSON(patch) != nil {
		return nil, InvalidBody
	}
	mutation, err := decodeMutation(patch)
	if err != nil {
		return nil, err
	}
	return applyMutation(stored, mutation, nil)
}

func applyMutation(stored json.RawMessage, mutation mutation, after func(map[string]json.RawMessage) error) (json.RawMessage, error) {
	document, _ := object(stored)
	for family, value := range mutation.fields {
		switch family {
		case "prefs":
			incoming, _ := object(value)
			if len(incoming) == 0 {
				continue
			}
			current, err := targetObject(document, family)
			if err != nil {
				return nil, err
			}
			document[family], err = mergePreferences(current, incoming)
			if err != nil {
				return nil, StorageUnavailable
			}
		case "favorites", "wantToWatch":
			op := mutation.maps[family]
			if len(op.set) == 0 && len(op.remove) == 0 {
				continue
			}
			current, err := targetObject(document, family)
			if err != nil {
				return nil, err
			}
			for id, entry := range op.set {
				current[id] = entry
			}
			for _, id := range op.remove {
				delete(current, id)
			}
			document[family], err = Encode(current)
			if err != nil {
				return nil, StorageUnavailable
			}
		case "playlists", "queue":
			document[family] = value
		}
		if family != "wantToWatch" && !compatible(family, document[family]) {
			return nil, StorageUnavailable
		}
	}
	if after != nil {
		if err := after(document); err != nil {
			return nil, err
		}
	}
	result, err := Encode(document)
	if err != nil {
		return nil, StorageUnavailable
	}
	if len(result) > MaxBlobBytes {
		return nil, TooLarge
	}
	if ValidateJSON(result) != nil {
		return nil, InvalidBody
	}
	return result, nil
}

func targetObject(document map[string]json.RawMessage, name string) (map[string]json.RawMessage, error) {
	raw, exists := document[name]
	if !exists {
		return map[string]json.RawMessage{}, nil
	}
	value, err := object(raw)
	if err != nil {
		return nil, StorageUnavailable
	}
	return value, nil
}

func mergePreferences(current, incoming map[string]json.RawMessage) ([]byte, error) {
	for key, value := range incoming {
		if next, err := object(value); err == nil {
			previous, err := object(current[key])
			if err != nil {
				previous = map[string]json.RawMessage{}
			}
			merged, err := mergePreferences(previous, next)
			if err != nil {
				return nil, err
			}
			current[key] = merged
		} else {
			current[key] = value
		}
	}
	return Encode(current)
}
