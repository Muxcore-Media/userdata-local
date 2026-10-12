package conditional

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const receiptLayout = "2006-01-02T15:04:05.000000000Z"

var observationPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`)
var progressFields = []string{"id", "positionSec", "durationSec", "watched", "updatedAt", "observedAt", "kind", "title", "href", "poster_url", "stream_url"}

type progressReport struct {
	position, duration json.RawMessage
	metadata           map[string]json.RawMessage
	observed           json.RawMessage
}

type progressOperation struct {
	reports map[string]progressReport
	watched map[string]bool
	remove  []string
}

func (op progressOperation) intent() bool {
	return len(op.reports) > 0 || len(op.watched) > 0 || len(op.remove) > 0
}

// validProgressNumber checks mathematical sign/zero using the original token,
// independently of float64 rounding. Unknown metadata numbers never pass here.
func validProgressNumber(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || ValidateJSON(raw) != nil || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return false
	}
	token := string(raw)
	value, err := strconv.ParseFloat(token, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return false
	}
	significand, _, _ := strings.Cut(strings.ToLower(token), "e")
	nonzero := strings.ContainsAny(significand, "123456789")
	return !nonzero || (raw[0] != '-' && value > 0)
}

func normalizeObservation(raw json.RawMessage) (json.RawMessage, bool) {
	var value string
	if json.Unmarshal(raw, &value) != nil || !observationPattern.MatchString(value) {
		return nil, false
	}
	// time.Parse accepts offset hour 24 and minute 60; the wire contract does not.
	if value[len(value)-1] != 'Z' {
		zone := value[len(value)-6:]
		hour, _ := strconv.Atoi(zone[1:3])
		minute, _ := strconv.Atoi(zone[4:])
		if hour > 23 || minute > 59 {
			return nil, false
		}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, false
	}
	utc := parsed.UTC()
	if utc.Year() < 0 || utc.Year() > 9999 {
		return nil, false
	}
	encoded, err := Encode(utc.Format(receiptLayout))
	return encoded, err == nil
}

func canonicalProgressField(name string) (string, bool) {
	for _, known := range progressFields {
		if strings.EqualFold(name, known) {
			return known, true
		}
	}
	return "", false
}

func decodeProgress(raw json.RawMessage) (progressOperation, error) {
	op := progressOperation{reports: map[string]progressReport{}, watched: map[string]bool{}}
	fields, err := object(raw)
	if err != nil || !exactFields(fields, nil, "report", "set_watched", "remove") {
		return op, InvalidBody
	}
	if value, exists := fields["report"]; exists {
		reports, err := object(value)
		if err != nil {
			return op, InvalidBody
		}
		for id, rawReport := range reports {
			report, err := object(rawReport)
			if !ValidID(id) || err != nil || !exactFields(report, []string{"positionSec", "durationSec"}, "metadata", "observedAt") ||
				!validProgressNumber(report["positionSec"]) || !validProgressNumber(report["durationSec"]) {
				return op, InvalidBody
			}
			parsed := progressReport{position: report["positionSec"], duration: report["durationSec"]}
			if metadata, exists := report["metadata"]; exists {
				parsed.metadata, err = object(metadata)
				if err != nil {
					return op, InvalidBody
				}
				for name := range parsed.metadata {
					if known, found := canonicalProgressField(name); found {
						switch known {
						case "id", "positionSec", "durationSec", "watched", "updatedAt", "observedAt":
							return op, InvalidBody
						}
						if name != known {
							return op, InvalidBody
						}
						if _, ok := stringField(parsed.metadata, name); !ok {
							return op, InvalidBody
						}
					}
				}
			}
			if observed, exists := report["observedAt"]; exists {
				var ok bool
				parsed.observed, ok = normalizeObservation(observed)
				if !ok {
					return op, InvalidBody
				}
			}
			op.reports[id] = parsed
		}
	}
	if value, exists := fields["set_watched"]; exists {
		watched, err := object(value)
		if err != nil {
			return op, InvalidBody
		}
		for id, rawBool := range watched {
			rawBool = bytes.TrimSpace(rawBool)
			if !ValidID(id) || (!bytes.Equal(rawBool, []byte("true")) && !bytes.Equal(rawBool, []byte("false"))) {
				return op, InvalidBody
			}
			op.watched[id] = bytes.Equal(rawBool, []byte("true"))
		}
	}
	if value, exists := fields["remove"]; exists {
		if !array(value) || json.Unmarshal(value, &op.remove) != nil {
			return op, InvalidBody
		}
		seen := map[string]bool{}
		for _, id := range op.remove {
			_, reported := op.reports[id]
			_, watched := op.watched[id]
			if !ValidID(id) || seen[id] || reported || watched {
				return op, InvalidBody
			}
			seen[id] = true
		}
	}
	return op, nil
}

func progressEntry(current map[string]json.RawMessage, id string) (map[string]json.RawMessage, error) {
	raw, exists := current[id]
	if !exists {
		encodedID, _ := Encode(id)
		return map[string]json.RawMessage{"id": encodedID, "positionSec": json.RawMessage("0"), "durationSec": json.RawMessage("0")}, nil
	}
	entry, err := object(raw)
	if err != nil {
		return nil, StorageUnavailable
	}
	for name := range entry {
		if known, found := canonicalProgressField(name); found && name != known {
			return nil, StorageUnavailable
		}
	}
	if _, exists := entry["id"]; exists {
		if value, valid := stringField(entry, "id"); !valid || value != id {
			return nil, StorageUnavailable
		}
	} else {
		entry["id"], _ = Encode(id)
	}
	return entry, nil
}

func validateProgressEntry(entry map[string]json.RawMessage) bool {
	for _, name := range []string{"positionSec", "durationSec"} {
		if value, exists := entry[name]; exists && !validProgressNumber(value) {
			return false
		}
	}
	if value, exists := entry["watched"]; exists && !isNull(value) {
		value = bytes.TrimSpace(value)
		if !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
			return false
		}
	}
	for _, name := range progressFields[6:] {
		if _, exists := entry[name]; exists {
			if _, ok := stringField(entry, name); !ok {
				return false
			}
		}
	}
	if value, exists := entry["observedAt"]; exists {
		if _, ok := normalizeObservation(value); !ok {
			return false
		}
	}
	return true
}

func applyProgress(current map[string]json.RawMessage, op progressOperation, receipt time.Time) error {
	affected := map[string]bool{}
	for id := range op.reports {
		affected[id] = true
	}
	for id := range op.watched {
		affected[id] = true
	}
	timestamp, err := Encode(receipt.UTC().Format(receiptLayout))
	if err != nil || receipt.UTC().Year() < 0 || receipt.UTC().Year() > 9999 {
		return StorageUnavailable
	}
	for id := range affected {
		entry, err := progressEntry(current, id)
		if err != nil {
			return err
		}
		if report, exists := op.reports[id]; exists {
			entry["positionSec"], entry["durationSec"] = report.position, report.duration
			for name, value := range report.metadata {
				entry[name] = value
			}
			delete(entry, "observedAt")
			if report.observed != nil {
				entry["observedAt"] = report.observed
			}
		}
		if watched, exists := op.watched[id]; exists {
			if watched {
				entry["watched"] = json.RawMessage("true")
			} else {
				entry["watched"] = json.RawMessage("false")
			}
		}
		if bytes.Equal(bytes.TrimSpace(entry["watched"]), []byte("true")) {
			entry["positionSec"] = json.RawMessage("0")
		}
		if !validateProgressEntry(entry) {
			return StorageUnavailable
		}
		entry["updatedAt"] = timestamp
		current[id], err = Encode(entry)
		if err != nil {
			return StorageUnavailable
		}
	}
	for _, id := range op.remove {
		delete(current, id)
	}
	return nil
}
