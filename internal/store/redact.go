package store

import (
	"encoding/json"
	"strings"
)

// RedactedValue replaces the value of a sensitive header before storage.
const RedactedValue = "[redacted]"

// sensitiveHeaders are lower-case names whose values must never be persisted.
var sensitiveHeaders = map[string]bool{
	"cookie":              true,
	"authorization":       true,
	"x-admin-key":         true,
	"proxy-authorization": true,
}

// RedactHeadersJSON takes a JSON object of header name -> value and replaces
// the values of sensitive headers (case-insensitive name match). Empty input
// is returned unchanged. If the input is not a JSON object it cannot be
// vetted, so "{}" is stored rather than risk persisting credentials.
func RedactHeadersJSON(in string) string {
	if strings.TrimSpace(in) == "" {
		return in
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(in), &m); err != nil || m == nil {
		return "{}"
	}
	changed := false
	for k := range m {
		if sensitiveHeaders[strings.ToLower(strings.TrimSpace(k))] {
			m[k] = json.RawMessage(`"` + RedactedValue + `"`)
			changed = true
		}
	}
	if !changed {
		return in
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(out)
}
