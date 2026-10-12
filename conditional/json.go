package conditional

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

const MaxDepth = 64

// Encode preserves raw numeric tokens and does not HTML-escape raw documents.
// Object order and whitespace are not part of the contract.
func Encode(value any) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

// ValidateJSON rejects JSON ambiguities which encoding/json otherwise accepts:
// duplicate decoded names, invalid UTF-8 and unpaired UTF-16 surrogate escapes.
func ValidateJSON(raw []byte) error {
	if !utf8.Valid(raw) || !validSurrogates(raw) {
		return errors.New("invalid JSON text encoding")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := jsonValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func jsonValue(dec *json.Decoder, depth int) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	if depth >= MaxDepth || (delim != '{' && delim != '[') {
		return errors.New("invalid JSON nesting")
	}
	names := map[string]struct{}{}
	for dec.More() {
		if delim == '{' {
			name, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := name.(string)
			if !ok {
				return errors.New("invalid object name")
			}
			if _, exists := names[key]; exists {
				return errors.New("duplicate object name")
			}
			names[key] = struct{}{}
		}
		if err := jsonValue(dec, depth+1); err != nil {
			return err
		}
	}
	end, err := dec.Token()
	if err != nil || (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
		return errors.New("invalid JSON container")
	}
	return nil
}

func validSurrogates(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func object(raw []byte) (map[string]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New("object required")
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func array(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '['
}

func stringField(fields map[string]json.RawMessage, name string) (string, bool) {
	raw, exists := fields[name]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func exactFields(fields map[string]json.RawMessage, required []string, optional ...string) bool {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, name := range required {
		if _, exists := fields[name]; !exists {
			return false
		}
		allowed[name] = true
	}
	for _, name := range optional {
		allowed[name] = true
	}
	for name := range fields {
		if !allowed[name] {
			return false
		}
	}
	return true
}
