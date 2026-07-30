// Small HTTP helpers shared by the REST handlers (auth.go, kvs.go) and the
// fault-injection control endpoint (control.go).
package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// maxRequestBody caps every request body this mock reads. Nothing it serves
// takes anything larger than a small JSON object; a caller sending more than
// this is a bug worth failing loudly on rather than a case worth buffering.
const maxRequestBody = 1 << 20 // 1 MiB

// readLimitedBody reads and closes r.Body, capped at maxRequestBody.
func readLimitedBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	limited := io.LimitReader(r.Body, maxRequestBody+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(b) > maxRequestBody {
		return nil, io.ErrShortBuffer
	}
	return b, nil
}

// writeJSON writes v as a JSON response body with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
