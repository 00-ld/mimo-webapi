package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// A reply that tried to call a tool and got the format wrong is the only case
// worth retrying.
func TestLooksLikeFailedCallDetectsAttempts(t *testing.T) {
	cases := []string{
		// The native syntax, broken in various ways.
		`<tool_call><function=shell><parameter=cmd>["ls"]</parameter>`,
		`<function=shell><parameter=cmd>ls</parameter></function>`,
		`<parameter=cmd>ls</parameter>`,
		`<tool_call|shell|{"cmd":["ls"]}`,
		// The JSON envelope, malformed.
		`{"tool":"shell","arguments":{"cmd":["ls"]}`,
		`{"tool_call": {"name": "shell"}}`,
		`{"tool":"shell","arguments":}`,
	}
	for _, reply := range cases {
		if !looksLikeFailedCall(reply) {
			t.Errorf("reply %q was not recognised as a failed call attempt", reply)
		}
	}
}

// Ordinary prose must never trigger a retry. Retrying a normal answer doubles
// the cost of every conversation for no benefit.
func TestLooksLikeFailedCallIgnoresProse(t *testing.T) {
	cases := []string{
		"The files are in /tmp.",
		"Here is the config:\n{\"debug\": true, \"port\": 8080}",
		"Use a function to do that.",
		"I would call the shell tool if I had one.",
		"",
		"   ",
		// A code sample mentioning the concept but not attempting a call.
		"```go\nfunc call() {}\n```",
	}
	for _, reply := range cases {
		if looksLikeFailedCall(reply) {
			t.Errorf("prose %q was mistaken for a failed call attempt", reply)
		}
	}
}

// A reply that parsed successfully must not be retried, even though it contains
// the same markers a broken one would.
func TestSuccessfulCallIsNotRetried(t *testing.T) {
	reply := `<tool_call><function=shell><parameter=cmd>["ls"]</parameter></function></tool_call>`
	calls, _ := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("the reply did not parse, so this test proves nothing")
	}
	// The guard is `len(calls) == 0 && looksLikeFailedCall(...)`, so a parsed
	// reply never reaches the detector.
	if len(calls) == 0 && looksLikeFailedCall(reply) {
		t.Error("a successfully parsed call would have been retried")
	}
}

// The correction must name the format precisely enough to be followed, and
// must offer only names the client declared.
func TestRetryInstructionRestatesTheFormat(t *testing.T) {
	for _, want := range []string{
		xmlToolOpen, "<function=", "<parameter=", `"tool"`, `"arguments"`,
	} {
		if !containsStr(toolRetryInstruction, want) {
			t.Errorf("correction is missing %q", want)
		}
	}
	// It must not promise a tool that was not declared.
	if containsStr(toolRetryInstruction, "nuke") {
		t.Error("correction invents a tool name")
	}
}

func containsStr(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// ---- retry behaviour, against a scripted upstream -------------------------

// scriptedUpstream serves a different frame script on each call, so a test can
// make the first attempt fail and the second succeed.
func scriptedUpstream(t *testing.T, scripts ...string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			n := int(atomic.AddInt32(&calls, 1)) - 1
			if n >= len(scripts) {
				n = len(scripts) - 1
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(scripts[n]))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// frames builds a minimal upstream script carrying one assistant message.
func frames(content string) string {
	payload, _ := json.Marshal(map[string]string{"content": content})
	return "event: message\ndata: " + string(payload) + "\n\n" +
		"event: finish\ndata: {\"content\":\"stop\"}\n\n"
}

// toolRequest is a chat completion carrying one declared tool.
var toolRequest = func() string {
	body := map[string]any{
		"model":    "mimo-v2.6-flash",
		"messages": []any{map[string]any{"role": "user", "content": "list /tmp"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "shell",
				"description": "run a command",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"cmd": map[string]any{"type": "array"}},
				},
			},
		}},
	}
	encoded, _ := json.Marshal(body)
	return string(encoded)
}()

// The whole point of the retry: a model that botches the format on the first
// attempt gets a second one, and the client sees a usable tool call.
func TestRetryRecoversFromMalformedFirstAttempt(t *testing.T) {
	// First attempt: a native call with the closing tag missing.
	bad := `<tool_call><function=shell><parameter=cmd>["ls","/tmp"]</parameter>`
	good := `<tool_call><function=shell><parameter=cmd>["ls","/tmp"]</parameter></function></tool_call>`
	up, calls := scriptedUpstream(t, frames(bad), frames(good))
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions", toolRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("upstream was called %d times, want 2", got)
	}

	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	ch := resp.Choices[0]
	if ch.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(ch.Message.ToolCalls))
	}
	if ch.Message.ToolCalls[0].Function.Name != "shell" {
		t.Errorf("name = %q", ch.Message.ToolCalls[0].Function.Name)
	}
}

// A well-formed first attempt must not trigger a second call: the retry costs
// a full upstream round trip, which is seconds on this backend.
func TestNoRetryWhenFirstAttemptSucceeds(t *testing.T) {
	good := `<tool_call><function=shell><parameter=cmd>["ls"]</parameter></function></tool_call>`
	up, calls := scriptedUpstream(t, frames(good), frames(good))
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions", toolRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("upstream was called %d times, want 1", got)
	}
}

// An ordinary prose answer must not be retried either.
func TestNoRetryForPlainAnswer(t *testing.T) {
	up, calls := scriptedUpstream(t, frames("There are three files in /tmp."))
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions", toolRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("upstream was called %d times, want 1", got)
	}
}

// A request that declared no tools cannot have a failed tool call, so it is
// never retried even when the reply contains marker-like text.
func TestNoRetryWithoutDeclaredTools(t *testing.T) {
	up, calls := scriptedUpstream(t, frames(`{"tool":"shell","arguments":`))
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions",
		`{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("upstream was called %d times, want 1", got)
	}
}

// If the second attempt is also malformed, the reply is returned as text
// rather than retried a third time. Retrying forever would stall the client.
func TestRetryGivesUpAfterOneAttempt(t *testing.T) {
	bad := `<tool_call><function=shell><parameter=cmd>["ls"]`
	up, calls := scriptedUpstream(t, frames(bad), frames(bad), frames(bad))
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions", toolRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("upstream was called %d times, want exactly 2", got)
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Choices[0].Message.Content == nil ||
		*resp.Choices[0].Message.Content == "" {
		t.Error("the failed reply was discarded instead of being shown as text")
	}
}

// The correction must actually be sent, or the second attempt is identical to
// the first and cannot succeed.
func TestRetrySendsTheCorrection(t *testing.T) {
	bad := `<tool_call><function=shell><parameter=cmd>["ls"]`
	good := `<tool_call><function=shell><parameter=cmd>["ls"]</parameter></function></tool_call>`

	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Query string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			queries = append(queries, body.Query)
			n := len(queries) - 1
			mu.Unlock()

			script := bad
			if n > 0 {
				script = good
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(frames(script)))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}))
	t.Cleanup(srv.Close)

	s := newTestServer(t, srv.URL)
	_ = doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions", toolRequest)

	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 {
		t.Fatalf("upstream saw %d queries, want 2", len(queries))
	}
	if !containsStr(queries[1], "Correction") {
		t.Errorf("the second query carries no correction:\n%s", queries[1])
	}
	if containsStr(queries[0], "Correction") {
		t.Errorf("the first query was already corrected:\n%s", queries[0])
	}
}

// The Responses endpoint must retry by the same rule.
func TestResponsesRetriesMalformedCall(t *testing.T) {
	bad := `<tool_call><function=shell><parameter=cmd>["ls"]`
	good := `<tool_call><function=shell><parameter=cmd>["ls"]</parameter></function></tool_call>`
	up, calls := scriptedUpstream(t, frames(bad), frames(good))
	s := newTestServer(t, up.URL)

	body := `{"model":"mimo-v2.6-flash","input":"list /tmp",` +
		`"tools":[{"type":"function","name":"shell","description":"run",` +
		`"parameters":{"type":"object","properties":{"cmd":{"type":"array"}}}}]}`
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/responses", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("upstream was called %d times, want 2", got)
	}

	var resp struct {
		Output []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) == 0 || resp.Output[0].Type != "function_call" {
		t.Errorf("output = %+v, want a function_call item", resp.Output)
	}
}
