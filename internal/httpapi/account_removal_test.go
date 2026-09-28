package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Removing an unknown account must say so rather than reporting the
// last-account rule, otherwise an operator chasing a typo is told to add an
// account instead of fixing the label.
func TestRemoveUnknownAccountIsNotFound(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodDelete, "/admin/api/auth/sessions/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// A relay with no account fails every request, so emptying the pool is a state
// the operator almost never wants and cannot easily notice.
func TestRemoveLastAccountIsRefused(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodDelete, "/admin/api/auth/sessions/test", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// The response must not echo cookie material.
func TestRemoveAccountDoesNotLeakCookies(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodDelete, "/admin/api/auth/sessions/nope", "")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, v := range body {
		if s, ok := v.(string); ok && len(s) > 200 {
			t.Errorf("response carries a long value that may be a credential: %d chars", len(s))
		}
	}
}
