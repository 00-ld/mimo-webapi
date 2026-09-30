package httpapi

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mimowebapi/internal/config"
	"mimowebapi/internal/upstream"
)

// tooLongFrame is the frame the live backend emits for an oversized query.
//
// It arrives inside an ordinary HTTP 200 SSE stream, which is what makes the
// condition so easy to misclassify: there is no status code to inspect.
const tooLongFrame = `event: error` + "\n" +
	`data: {"content":"抱歉，您发送的文本超长啦！建议您适当简化内容，或者分段发送。感谢您的理解"}` + "\n\n"

// The pre-flight guard must reject an oversized query before spending an
// upstream round-trip, with a status and code a client can act on.
//
// 400 + context_length_exceeded is the contract: an agent harness branches on
// that code to compact its history, while a 502 would make it retry the
// identical payload forever.
func TestOversizedQueryRejectedBeforeUpstream(t *testing.T) {
	var upstreamCalls int
	upstreamSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			upstreamCalls++
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("event: message\ndata: {\"content\":\"hi\"}\n\n"))
		}))
	defer upstreamSrv.Close()

	cfg := testConfig(upstreamSrv.URL)
	cfg.Upstream.MaxQueryChars = 100
	srv, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash",
		"messages": []map[string]string{{"role": "user", "content": strings.Repeat("x", 500)}},
	})
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/chat/completions", string(body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("undecodable error: %v (%s)", err, rec.Body.String())
	}
	if got.Error.Code != contextTooLongCode {
		t.Errorf("code = %q, want %q", got.Error.Code, contextTooLongCode)
	}
	if got.Error.Param != "messages" {
		t.Errorf("param = %q, want messages", got.Error.Param)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstream was called %d times; the guard must reject locally",
			upstreamCalls)
	}
}

// A CJK prompt must be measured in characters, not bytes.
//
// A Chinese character is three UTF-8 bytes, so a byte-based guard would reject
// a request the backend accepts and let an ASCII one through that it does not.
func TestOversizedGuardCountsCharactersNotBytes(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("event: message\ndata: {\"content\":\"ok\"}\n\n"))
		}))
	defer upstreamSrv.Close()

	cfg := testConfig(upstreamSrv.URL)
	cfg.Upstream.MaxQueryChars = 50
	srv, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// 30 CJK characters is 90 bytes but only 30 characters, which is under the
	// 50-character ceiling and must be accepted.
	body, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash",
		"messages": []map[string]string{{"role": "user", "content": strings.Repeat("测", 30)}},
	})
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/chat/completions", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("30 CJK chars under a 50-char ceiling was rejected: %d %s",
			rec.Code, rec.Body.String())
	}
}

// The guard is off when the ceiling is zero, so an operator can defer entirely
// to the backend.
func TestOversizedGuardDisabledAtZero(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("event: message\ndata: {\"content\":\"ok\"}\n\n"))
		}))
	defer upstreamSrv.Close()

	cfg := testConfig(upstreamSrv.URL)
	cfg.Upstream.MaxQueryChars = 0
	srv, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash",
		"messages": []map[string]string{{"role": "user", "content": strings.Repeat("x", 5000)}},
	})
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/chat/completions", string(body))
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("guard fired with the ceiling disabled: %s", rec.Body.String())
	}
}

// An error frame that reaches a streaming client must be machine-readable.
//
// The status line is already 200 when the frame arrives, so the failure can
// only travel in-band. Without a typed event the client sees an empty reply
// and retries, which is the loop this whole change exists to break.
func TestStreamingUpstreamErrorCarriesCode(t *testing.T) {
	srv := newTestServer(t, mockUpstream(t, []string{tooLongFrame}).URL)

	body, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/chat/completions", string(body))

	out := rec.Body.String()
	if !strings.Contains(out, contextTooLongCode) {
		t.Fatalf("stream did not surface %q; got:\n%s", contextTooLongCode, out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("stream was not terminated cleanly; got:\n%s", out)
	}
}

// A non-streaming oversized rejection must not be reported as a retryable 502.
func TestNonStreamingUpstreamTooLongIsNot502(t *testing.T) {
	srv := newTestServer(t, mockUpstream(t, []string{tooLongFrame}).URL)

	body, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/chat/completions", string(body))

	if rec.Code == http.StatusBadGateway {
		t.Fatalf("oversized request reported as retryable 502: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), contextTooLongCode) {
		t.Errorf("missing %q in %s", contextTooLongCode, rec.Body.String())
	}
}

// The Anthropic surface must report the same condition in its own envelope,
// using an inner error type its clients recognise.
func TestAnthropicTooLongEnvelope(t *testing.T) {
	srv := newTestServer(t, mockUpstream(t, []string{tooLongFrame}).URL)

	body, _ := json.Marshal(map[string]any{
		"model":      "mimo-v2.6-flash",
		"max_tokens": 128,
		"messages":   []map[string]string{{"role": "user", "content": "hello"}},
	})
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/messages", string(body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("undecodable: %v (%s)", err, rec.Body.String())
	}
	if got.Type != "error" || got.Error.Type == "" {
		t.Errorf("malformed Anthropic envelope: %s", rec.Body.String())
	}
}

// The Responses surface must carry the code in its error event.
func TestResponsesTooLongCode(t *testing.T) {
	srv := newTestServer(t, mockUpstream(t, []string{tooLongFrame}).URL)

	body, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash",
		"input":    "hello",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/responses", string(body))

	out := rec.Body.String()
	if !strings.Contains(out, contextTooLongCode) {
		t.Fatalf("responses stream did not surface %q; got:\n%s", contextTooLongCode, out)
	}
}

// Classification is shared, so the unit-level behaviour is pinned here too.
func TestUpstreamErrorShapeClassification(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"too long", upstream.NewQueryTooLong("文本超长", 120000, 95000),
			http.StatusBadRequest, contextTooLongCode},
		{"banned", &upstream.BackendError{Status: 461, Body: "banned"},
			http.StatusBadGateway, "account_banned"},
		{"auth", &upstream.BackendError{Status: 401, Body: "nope"},
			http.StatusBadGateway, "cookies_expired"},
		{"generic", &upstream.BackendError{Status: 500, Body: "boom"},
			http.StatusBadGateway, "upstream_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, msg, _ := upstreamErrorShape(tc.err, 95000)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Errorf("got (%d,%s), want (%d,%s)", status, code, tc.wantStatus, tc.wantCode)
			}
			if msg == "" {
				t.Error("message must not be empty")
			}
		})
	}
}

// The marker list must recognise the live wording, in both languages.
func TestLooksQueryTooLong(t *testing.T) {
	yes := []string{
		"抱歉，您发送的文本超长啦！建议您适当简化内容，或者分段发送。感谢您的理解",
		"This model's maximum context length is exceeded",
		"text too long",
	}
	for _, s := range yes {
		if !upstream.LooksQueryTooLong(s) {
			t.Errorf("not detected as too long: %q", s)
		}
	}
	no := []string{"internal server error", "rate limited", ""}
	for _, s := range no {
		if upstream.LooksQueryTooLong(s) {
			t.Errorf("false positive: %q", s)
		}
	}
}

// The default ceiling must stay below the measured live boundary.
func TestDefaultMaxQueryCharsHasMargin(t *testing.T) {
	if config.DefaultMaxQueryChars >= 100000 {
		t.Errorf("default ceiling %d is at or above the measured 100k boundary",
			config.DefaultMaxQueryChars)
	}
	cfg := config.Default()
	if cfg.Upstream.MaxQueryChars != config.DefaultMaxQueryChars {
		t.Errorf("Default() did not populate max_query_chars: %d",
			cfg.Upstream.MaxQueryChars)
	}
}

// truncatingUpstream sends a partial stream and then kills the connection
// without a finish frame, which is what a network drop or an upstream restart
// looks like from the relay's side.
func truncatingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// Announce chunked framing so a missing terminator is detectable, then
		// send one frame and abort without the closing chunk.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/event-stream\r\n" +
			"Transfer-Encoding: chunked\r\n\r\n")
		payload := "event: message\ndata: {\"content\":\"partial answer\"}\n\n"
		_, _ = buf.WriteString(fmt.Sprintf("%x\r\n%s\r\n", len(payload), payload))
		_ = buf.Flush()
		// An ungraceful close makes the read fail instead of ending cleanly.
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A stream that dies mid-generation must never be reported to the client as a
// completed reply.
//
// Without truncation detection the relay returned HTTP 200 with
// finish_reason "stop" for a half-generated answer, so an agent treated a
// broken connection as the model finishing its thought.
func TestTruncatedStreamIsNotReportedAsComplete(t *testing.T) {
	srv := newTestServer(t, truncatingUpstream(t).URL)

	body, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/chat/completions", string(body))

	if rec.Code == http.StatusOK {
		t.Fatalf("truncated stream reported as a successful completion (HTTP 200): %s",
			rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Errorf("truncated reply presented as finished: %s", rec.Body.String())
	}
}

// The streaming variant must surface the truncation before terminating.
func TestTruncatedStreamEmitsErrorEvent(t *testing.T) {
	srv := newTestServer(t, truncatingUpstream(t).URL)

	body, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	rec := doJSON(t, srv.Routes(), http.MethodPost, "/v1/chat/completions", string(body))

	out := rec.Body.String()
	if !strings.Contains(out, `"error"`) {
		t.Fatalf("truncated stream produced no error event: %s", out)
	}
}
