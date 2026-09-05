package relay

import (
	"encoding/json"
	"net/http"
)

// jsonError writes an error response body {"error": msg} with the given status
// and a Content-Type of application/json.
//
// It exists because a hand-built `{"error":%q}` literal is NOT valid JSON: Go's
// %q verb (and strconv.Quote) uses Go string escaping, so a control byte in msg
// is emitted as \x1b and a rune above 0x7f may become \u{...} — neither of which
// JSON accepts. The console then fails res.json() and swallows the reason for
// every caller. Marshalling a map through encoding/json escapes any value —
// quotes, backslashes, control bytes — the one correct way, so the refusal
// reason always reaches the client. Unlike http.Error, the body is served as
// application/json rather than text/plain.
func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		// Unreachable for a string value, but never emit a partial/invalid body.
		b = []byte(`{"error":"internal error"}`)
	}
	_, _ = w.Write(b)
}
