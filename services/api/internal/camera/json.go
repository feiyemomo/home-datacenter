package camera

import "encoding/json"

// mustJSON marshals `v` and panics on error. It is only used for
// emit-only payloads (event bus, logs) where a marshal failure is a
// programmer bug, not a runtime concern.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
