package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The `thinking` field is not part of the OpenAI specification, so clients
// invented incompatible encodings. Typing it as *bool made the object form a
// decode error that failed the entire request:
//
//	json: cannot unmarshal object into Go struct field
//	ChatCompletionRequest.thinking of type bool
//
// That is a hard failure for a field the relay can simply interpret, and it
// was reported by ZCode, which sends the Anthropic-style object.
func TestThinkingFieldAcceptsEveryClientEncoding(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bare true", `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"thinking":true}`},
		{"bare false", `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"thinking":false}`},
		{"anthropic enabled", `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`},
		{"anthropic disabled", `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`},
		{"anthropic with budget", `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096}}`},
		{"object enabled flag", `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"enabled":true}}`},
		{"empty object", `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"thinking":{}}`},
		{"quoted string", `{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"thinking":"enabled"}`},
	}

	srv := newTestServer(t, mockUpstream(t, []string{
		"event: message\ndata: {\"content\":\"ok\"}\n\n",
		"event: finish\ndata: {}\n\n",
	}).URL)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/chat/completions", tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "cannot unmarshal") {
				t.Fatalf("decode error leaked to the client: %s", rec.Body.String())
			}
		})
	}
}

// The same field on the Responses surface must accept both shapes.
func TestResponsesThinkingFieldAcceptsObject(t *testing.T) {
	srv := newTestServer(t, mockUpstream(t, []string{
		"event: message\ndata: {\"content\":\"ok\"}\n\n",
		"event: finish\ndata: {}\n\n",
	}).URL)

	body := `{"model":"mimo-v2.6-flash","input":"hi","thinking":{"type":"enabled"}}`
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/responses", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// The parser itself, pinned value by value.
func TestParseThinkingFlag(t *testing.T) {
	cases := []struct {
		in     string
		wantOn bool
		wantOk bool
	}{
		{`true`, true, true},
		{`false`, false, true},
		{`{"type":"enabled"}`, true, true},
		{`{"type":"disabled"}`, false, true},
		{`{"type":"on"}`, true, true},
		{`{"type":"OFF"}`, false, true},
		{`{"type":"enabled","budget_tokens":2048}`, true, true},
		{`{"enabled":true}`, true, true},
		{`{"enabled":false}`, false, true},
		{`{}`, true, true},
		{`"enabled"`, true, true},
		{`"off"`, false, true},
		{``, false, false},
		{`null`, false, false},
		{`123`, false, false},
	}
	for _, tc := range cases {
		on, ok := parseThinkingFlag(json.RawMessage(tc.in))
		if on != tc.wantOn || ok != tc.wantOk {
			t.Errorf("parseThinkingFlag(%s) = (%v,%v), want (%v,%v)",
				tc.in, on, ok, tc.wantOn, tc.wantOk)
		}
	}
}

// The Anthropic surface must keep working, since it shares the parser now.
func TestAnthropicThinkingStillWorks(t *testing.T) {
	srv := newTestServer(t, mockUpstream(t, []string{
		"event: message\ndata: {\"content\":\"ok\"}\n\n",
		"event: finish\ndata: {}\n\n",
	}).URL)

	for _, form := range []string{`{"type":"enabled"}`, `{"type":"disabled"}`, `true`, `false`} {
		body := `{"model":"mimo-v2.6-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thinking":` + form + `}`
		rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/messages", body)
		if rec.Code != http.StatusOK {
			t.Errorf("thinking=%s -> status %d: %s", form, rec.Code, rec.Body.String())
		}
	}
}
