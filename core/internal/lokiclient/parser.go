package lokiclient

import (
	"encoding/json"
)

// parseLogLine extracts structured fields from a JSON log line.
// Returns (level, msg, fieldsJSON) where fieldsJSON contains all other fields.
func parseLogLine(line string) (level, msg, fieldsJSON string) {
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		// Not JSON - return raw line as msg
		return "", line, ""
	}

	// Extract level and msg
	if l, ok := raw["level"].(string); ok {
		level = l
		delete(raw, "level")
	}
	if m, ok := raw["msg"].(string); ok {
		msg = m
		delete(raw, "msg")
	}

	// Remove timestamp fields (already extracted from Loki's metadata)
	delete(raw, "time")
	delete(raw, "ts")
	delete(raw, "timestamp")

	// Marshal remaining fields as JSON
	if len(raw) > 0 {
		if fields, err := json.Marshal(raw); err == nil {
			fieldsJSON = string(fields)
		}
	}

	return level, msg, fieldsJSON
}
