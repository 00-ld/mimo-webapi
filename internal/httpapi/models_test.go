package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Aggregators probe several spellings before deciding an upstream is usable.
// A 404 on any of them reads as "no models here" and the account is rejected.
func TestModelsProbePaths(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)
	routes := s.Routes()

	for _, path := range []string{
		"/v1/models", "/models",
		"/v1/models/mimo-v2.6-pro", "/models/mimo-v2.6-pro",
	} {
		t.Run(path, func(t *testing.T) {
			rec := doJSON(t, routes, http.MethodGet, path, "")
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s returned 404; aggregators treat that as no models", path)
			}
		})
	}
}

// The listing must be byte-stable across calls. A created timestamp that moves
// makes diffing clients re-sync on every poll.
func TestModelsListingIsStable(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)
	routes := s.Routes()

	first := doJSON(t, routes, http.MethodGet, "/v1/models", "").Body.String()
	second := doJSON(t, routes, http.MethodGet, "/v1/models", "").Body.String()
	if first != second {
		t.Error("model listing changed between identical calls")
	}
}

// Every listed model needs the descriptive fields a picker renders, otherwise
// the entry shows as a bare id with no context information.
func TestModelsCarryPickerMetadata(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)
	rec := doJSON(t, s.Routes(), http.MethodGet, "/v1/models", "")

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID            string   `json:"id"`
			Object        string   `json:"object"`
			OwnedBy       string   `json:"owned_by"`
			ContextLength int      `json:"context_length"`
			MaxOutput     int      `json:"max_output"`
			Capabilities  []string `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	if len(resp.Data) == 0 {
		t.Fatal("no models listed")
	}
	for _, m := range resp.Data {
		if m.Object != "model" {
			t.Errorf("%s: object = %q, want model", m.ID, m.Object)
		}
		if m.OwnedBy == "" {
			t.Errorf("%s: owned_by is empty", m.ID)
		}
		if m.ContextLength == 0 {
			t.Errorf("%s: context_length missing; the picker shows a blank cell", m.ID)
		}
		if len(m.Capabilities) == 0 {
			t.Errorf("%s: capabilities missing", m.ID)
		}
	}
}

// An unknown id is a real 404 rather than an empty success, so a client can
// tell "not available" from "upstream is broken".
func TestUnknownModelIsNotFound(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)
	rec := doJSON(t, s.Routes(), http.MethodGet, "/v1/models/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
