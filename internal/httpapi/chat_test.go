package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockUpstream stands in for the MiMo web backend: it answers the config
// endpoint and replays a fixed named-event SSE stream on chat.
func mockUpstream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/bot/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") == "" {
			http.Error(w, `{"code":401,"loginUrl":"https://account.xiaomi.com/x"}`,
				http.StatusUnauthorized)
			return
		}
		// The request must look like the web client's own payload.
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("upstream got undecodable body: %v", err)
		}
		for _, k := range []string{"msgId", "conversationId", "query", "modelConfig"} {
			if _, ok := body[k]; !ok {
				t.Errorf("upstream payload missing %q: %v", k, body)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = w.Write([]byte(f))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	cfg := testConfig(upstreamURL)
	srv, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const happyStream = "event: dialogId\ndata: {\"content\":\"dlg-1\"}\n\n" +
	"event: message\ndata: {\"content\":\"Hello\"}\n\n" +
	"event: message\ndata: {\"content\":\", world\"}\n\n" +
	"event: usage\ndata: {\"prompt_tokens\":7,\"completion_tokens\":3}\n\n" +
	"event: finish\ndata: {}\n\n"

func TestChatCompletionsNonStreaming(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions",
		`{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello, world" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v", resp.Choices[0].FinishReason)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q", resp.Object)
	}
}

func TestChatCompletionsStreaming(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions",
		`{"model":"mimo-v2.6-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	body := rec.Body.String()

	// The stream must open with a role-only chunk, then carry the text, then
	// terminate with the literal [DONE] sentinel OpenAI clients look for.
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Errorf("missing opening role chunk:\n%s", body)
	}
	if !strings.Contains(body, `"content":"Hello"`) ||
		!strings.Contains(body, `"content":", world"`) {
		t.Errorf("missing text deltas:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("missing finish reason:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE] sentinel:\n%s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":7`) {
		t.Errorf("missing usage in final chunk:\n%s", body)
	}
}

func TestChatCompletionsRejectsBadRequests(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)
	routes := s.Routes()

	cases := []struct {
		name string
		body string
	}{
		{"not json", `{`},
		{"no messages", `{"model":"m","messages":[]}`},
		{"empty content", `{"model":"m","messages":[{"role":"user","content":"  "}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, routes, http.MethodPost, "/v1/chat/completions", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// An unauthenticated caller must be rejected before any upstream work happens.
func TestAuthIsEnforced(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_api_key") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestAuthAcceptsAPIKeyHeader(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", testToken)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestModelsEndpoint(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)
	rec := doJSON(t, s.Routes(), http.MethodGet, "/v1/models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) == 0 {
		t.Errorf("unexpected payload: %s", rec.Body.String())
	}
}

// When upstream reports a content block, the finish reason must say so rather
// than masquerading as a normal stop.
func TestSensitiveQueryMapsToContentFilter(t *testing.T) {
	up := mockUpstream(t, []string{
		"event: sensitive_query\ndata: {\"content\":\"blocked\"}\n\n" +
			"event: finish\ndata: {}\n\n",
	})
	s := newTestServer(t, up.URL)
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	var resp ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := resp.Choices[0].FinishReason
	if got == nil || *got != "content_filter" {
		t.Errorf("finish_reason = %v, want content_filter", got)
	}
}

// A rejected cookie must surface as an upstream auth error, not a 200 with
// empty text.
func TestExpiredCookiesSurfaceAsUpstreamError(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)
	// Strip the session so the mock's cookie guard trips.
	s.client = nil
	s2, err := buildServerNoSessions(testConfig(up.URL))
	if err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, s2.Routes(), http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_session") {
		t.Errorf("body = %s", rec.Body.String())
	}
}
