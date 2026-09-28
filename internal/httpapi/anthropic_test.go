package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicMessagesNonStreaming(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)

	body := `{"model":"mimo-v2.6-flash","max_tokens":256,
	  "system":"be brief",
	  "messages":[{"role":"user","content":"hi"}]}`
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "message" || resp.Role != "assistant" {
		t.Errorf("envelope = %+v", resp)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello, world" {
		t.Errorf("content = %+v", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// Anthropic clients such as Claude Code assert on the streaming event order,
// so it is checked explicitly rather than treated as an implementation detail.
func TestAnthropicMessagesStreamingEventOrder(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)

	body := `{"model":"mimo-v2.6-flash","max_tokens":256,"stream":true,
	  "messages":[{"role":"user","content":"hi"}]}`
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := rec.Body.String()

	order := []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	}
	last := -1
	for _, ev := range order {
		idx := strings.Index(out, ev)
		if idx < 0 {
			t.Fatalf("missing %q in:\n%s", ev, out)
		}
		if idx < last {
			t.Errorf("event %q out of order", ev)
		}
		last = idx
	}
	if !strings.Contains(out, `"text_delta"`) {
		t.Errorf("missing text_delta:\n%s", out)
	}
	if !strings.Contains(out, `"text":"Hello"`) {
		t.Errorf("missing streamed text:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("missing stop_reason:\n%s", out)
	}
}

// Anthropic content blocks must be flattened, since the web protocol has no
// block structure to map them onto.
func TestAnthropicContentBlocksAreFlattened(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)

	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user",
	  "content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}]}`
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAnthropicRejectsEmptyMessages(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages",
		`{"model":"m","max_tokens":16,"messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestAnthropicRequiresAuth(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"x"}]}`))
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

// thinking must round-trip: an explicit disable must reach the upstream payload
// rather than being silently dropped.
func TestThinkingFlagReachesUpstream(t *testing.T) {
	var captured map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/bot/chat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(happyStream))
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	s := newTestServer(t, up.URL)
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions",
		`{"model":"m","thinking":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	mc, ok := captured["modelConfig"].(map[string]any)
	if !ok {
		t.Fatalf("no modelConfig in upstream payload: %+v", captured)
	}
	if mc["enableThinking"] != false {
		t.Errorf("enableThinking = %v, want false", mc["enableThinking"])
	}
}

// system_prompt_mode=drop must remove the system prompt before it reaches
// upstream, which is what clients relying on it expect.
func TestSystemPromptDropMode(t *testing.T) {
	var captured map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/bot/chat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(happyStream))
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	cfg := testConfig(up.URL)
	cfg.Behavior.SystemPromptMode = "drop"
	s, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"system","content":"MARKER-XYZ"},
		 {"role":"user","content":"hi"},{"role":"assistant","content":"yo"},
		 {"role":"user","content":"again"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	q, _ := captured["query"].(string)
	if strings.Contains(q, "MARKER-XYZ") {
		t.Errorf("system prompt leaked into upstream query:\n%s", q)
	}
	if !strings.Contains(q, "again") {
		t.Errorf("final turn missing from query:\n%s", q)
	}
}

// ---- tool calling over the Anthropic protocol ------------------------------

// anthropicToolRequest declares one tool in the Anthropic nesting and asks a
// question. input_schema rather than parameters is the field under test.
const anthropicToolRequest = `{"model":"mimo-v2.6-flash","max_tokens":256,
  "tools":[{"name":"shell","description":"run a command",
    "input_schema":{"type":"object","properties":{"cmd":{"type":"array"}}}}],
  "messages":[{"role":"user","content":"list /tmp"}]}`

// The Anthropic tool_use block is the whole point of the adapter: without it
// Claude Code sees prose where it expected a call and stalls.
func TestAnthropicToolUseBlock(t *testing.T) {
	reply := `<tool_call><function=shell><parameter=cmd>["ls","/tmp"]</parameter></function></tool_call>`
	up := mockUpstream(t, []string{frames(reply)})
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", anthropicToolRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}
	var block *struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	for i := range resp.Content {
		if resp.Content[i].Type == "tool_use" {
			block = &resp.Content[i]
		}
	}
	if block == nil {
		t.Fatalf("no tool_use block in %s", rec.Body.String())
	}
	if block.Name != "shell" {
		t.Errorf("name = %q", block.Name)
	}
	if block.ID == "" {
		t.Error("tool_use block has no id, so the result cannot be paired to it")
	}
	// input must be an object, not a string containing JSON: a client that
	// decodes it into a map fails outright on the string form.
	var args map[string]any
	if err := json.Unmarshal(block.Input, &args); err != nil {
		t.Fatalf("input is not an object: %v (%s)", err, block.Input)
	}
	cmd, ok := args["cmd"].([]any)
	if !ok || len(cmd) != 2 || cmd[0] != "ls" {
		t.Errorf("cmd = %v", args["cmd"])
	}
}

// The tool definitions must reach the upstream prompt, or the model never
// learns the tool exists and answers in prose forever.
func TestAnthropicToolsReachThePrompt(t *testing.T) {
	var captured map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/bot/chat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(happyStream))
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	s := newTestServer(t, up.URL)
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", anthropicToolRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	q, _ := captured["query"].(string)
	for _, want := range []string{"shell", "run a command", "cmd", `"tool"`} {
		if !strings.Contains(q, want) {
			t.Errorf("prompt is missing %q:\n%s", want, q)
		}
	}
}

// A request with no tools must not carry the tool instructions, so an ordinary
// conversation is unaffected by the adapter existing.
func TestAnthropicNoToolsNoPromptBlock(t *testing.T) {
	var captured map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/bot/chat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(happyStream))
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	s := newTestServer(t, up.URL)
	doJSON(t, s.Routes(), http.MethodPost, "/v1/messages",
		`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)

	q, _ := captured["query"].(string)
	if strings.Contains(q, "# Tools") {
		t.Errorf("tool instructions leaked into a request that declared no tools:\n%s", q)
	}
}

// The round trip: a tool_result on the next turn must reach the model, or it
// calls the same tool again forever.
func TestAnthropicToolResultReachesThePrompt(t *testing.T) {
	var captured map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/bot/chat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(happyStream))
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	s := newTestServer(t, up.URL)
	body := `{"model":"m","max_tokens":256,
	  "tools":[{"name":"shell","description":"run","input_schema":{"type":"object"}}],
	  "messages":[
	    {"role":"user","content":"list /tmp"},
	    {"role":"assistant","content":[
	      {"type":"text","text":"Checking."},
	      {"type":"tool_use","id":"toolu_01","name":"shell","input":{"cmd":["ls","/tmp"]}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_01","content":"alpha.txt\nbeta.log"}
	    ]}
	  ]}`
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	q, _ := captured["query"].(string)
	for _, want := range []string{"alpha.txt", "beta.log", "toolu_01", "shell"} {
		if !strings.Contains(q, want) {
			t.Errorf("prompt is missing %q:\n%s", want, q)
		}
	}
	if !strings.Contains(q, "Checking.") {
		t.Errorf("the assistant text block was dropped:\n%s", q)
	}
}

// A tool_result whose content is a block array rather than a string must still
// render, since that is the shape clients send for structured output.
func TestAnthropicToolResultBlockArray(t *testing.T) {
	body := `{"model":"m","max_tokens":64,
	  "messages":[
	    {"role":"assistant","content":[
	      {"type":"tool_use","id":"t1","name":"shell","input":{"cmd":["pwd"]}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"t1",
	       "content":[{"type":"text","text":"MARKER-STRUCTURED"}]}]}
	  ]}`
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	_, finalQuery, err := anthropicTurns(req, "auto")
	if err != nil {
		t.Fatalf("anthropicTurns: %v", err)
	}
	if !strings.Contains(finalQuery, "MARKER-STRUCTURED") {
		t.Errorf("structured tool result was dropped:\n%s", finalQuery)
	}
}

// An error result has to be labelled, or the model reads it as success.
func TestAnthropicToolResultErrorIsLabelled(t *testing.T) {
	body := `{"model":"m","max_tokens":64,
	  "messages":[
	    {"role":"assistant","content":[
	      {"type":"tool_use","id":"t1","name":"shell","input":{}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"t1","is_error":true,
	       "content":"command not found"}]}
	  ]}`
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	_, finalQuery, err := anthropicTurns(req, "auto")
	if err != nil {
		t.Fatalf("anthropicTurns: %v", err)
	}
	if !strings.Contains(finalQuery, "tool error") {
		t.Errorf("an error result is indistinguishable from a success:\n%s", finalQuery)
	}
}

// A conversation of nothing but tool traffic must still produce a usable turn
// rather than the "no content" error.
func TestAnthropicToolOnlyConversationIsValid(t *testing.T) {
	body := `{"model":"m","max_tokens":64,
	  "messages":[{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"t1","content":"output"}]}]}`
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if _, _, err := anthropicTurns(req, "auto"); err != nil {
		t.Errorf("anthropicTurns rejected a tool-only conversation: %v", err)
	}
}

// A tool_use block carries no text, so a naive flatten loses the record of
// what the assistant asked for.
func TestAnthropicToolUseBlockIsRestated(t *testing.T) {
	body := `{"model":"m","max_tokens":64,
	  "messages":[{"role":"assistant","content":[
	    {"type":"tool_use","id":"t1","name":"shell","input":{"cmd":["ls"]}}]}]}`
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	history, finalQuery, err := anthropicTurns(req, "auto")
	if err != nil {
		t.Fatalf("anthropicTurns: %v", err)
	}
	whole := flattenTurns(history) + finalQuery
	for _, want := range []string{"shell", "called tools"} {
		if !strings.Contains(whole, want) {
			t.Errorf("the requested call is absent from the prompt (%q):\n%s", want, whole)
		}
	}
}

// The streaming shape a tool call must take. Claude Code accumulates the
// input_json_delta fragments and parses the result, so the block has to open
// before the delta and close after it.
func TestAnthropicStreamingToolUse(t *testing.T) {
	reply := `<tool_call><function=shell><parameter=cmd>["ls"]</parameter></function></tool_call>`
	up := mockUpstream(t, []string{frames(reply)})
	s := newTestServer(t, up.URL)

	body := `{"model":"m","max_tokens":256,"stream":true,
	  "tools":[{"name":"shell","description":"run","input_schema":{"type":"object"}}],
	  "messages":[{"role":"user","content":"list /tmp"}]}`
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := rec.Body.String()

	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("no tool_use content block in:\n%s", out)
	}
	if !strings.Contains(out, `"type":"input_json_delta"`) {
		t.Fatalf("no input_json_delta in:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason is not tool_use in:\n%s", out)
	}

	// The block events must nest correctly: start, then delta, then stop, all
	// carrying the same index.
	startIdx := strings.Index(out, `"content_block_start"`)
	deltaIdx := strings.Index(out, `"input_json_delta"`)
	stopIdx := strings.Index(out[startIdx:], `"content_block_stop"`)
	if startIdx < 0 || deltaIdx < 0 || stopIdx < 0 || !(startIdx < deltaIdx) {
		t.Errorf("block events are out of order:\n%s", out)
	}
	// message_stop must be last.
	if strings.Index(out, "event: message_stop") < strings.Index(out, "event: message_delta") {
		t.Errorf("message_stop precedes message_delta:\n%s", out)
	}
	// The call markup must never leak as text.
	if strings.Contains(out, "tool_call&gt;") || strings.Contains(out, `"text":"<tool_call>"`) {
		t.Errorf("call markup leaked into text:\n%s", out)
	}
}

// A streaming reply that is pure prose must be unchanged by the tool layer
// existing: same event order, no tool blocks.
func TestAnthropicStreamingProseUnaffected(t *testing.T) {
	up := mockUpstream(t, []string{happyStream})
	s := newTestServer(t, up.URL)

	body := `{"model":"m","max_tokens":64,"stream":true,
	  "tools":[{"name":"shell","description":"run","input_schema":{"type":"object"}}],
	  "messages":[{"role":"user","content":"just say hi"}]}`
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", body)
	out := rec.Body.String()

	if strings.Contains(out, "tool_use") {
		t.Errorf("a prose reply produced a tool block:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason = not end_turn:\n%s", out)
	}
	for _, ev := range []string{
		"event: message_start", "event: content_block_start",
		"event: content_block_delta", "event: content_block_stop",
		"event: message_delta", "event: message_stop",
	} {
		if !strings.Contains(out, ev) {
			t.Errorf("missing %q in:\n%s", ev, out)
		}
	}
}

// A prose reply that also mentions a tool must stay prose: the parser only
// accepts a name the request declared AND a recognisable call syntax.
func TestAnthropicProseReplyIsNotACall(t *testing.T) {
	up := mockUpstream(t, []string{frames("I would use the shell tool, but not now.")})
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", anthropicToolRequest)
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	for _, b := range resp.Content {
		if b.Type == "tool_use" {
			t.Errorf("prose was mistaken for a tool call: %s", rec.Body.String())
		}
	}
	if len(resp.Content) == 0 || !strings.Contains(resp.Content[0].Text, "would use") {
		t.Errorf("the prose was lost: %s", rec.Body.String())
	}
}

// A malformed first attempt is retried, and the client sees a usable call
// rather than the failed markup as text.
func TestAnthropicRetryRecoversFromMalformedCall(t *testing.T) {
	bad := `<tool_call><function=shell><parameter=cmd>["ls"]</parameter>`
	good := `<tool_call><function=shell><parameter=cmd>["ls"]</parameter></function></tool_call>`
	up, calls := scriptedUpstream(t, frames(bad), frames(good))
	s := newTestServer(t, up.URL)

	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", anthropicToolRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if *calls < 2 {
		t.Errorf("the relay did not retry (upstream calls = %d)", *calls)
	}
	if !strings.Contains(rec.Body.String(), `"type":"tool_use"`) {
		t.Errorf("the retry did not produce a tool_use block:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason is not tool_use:\n%s", rec.Body.String())
	}
}

// A reply with no recognisable call and no failed-attempt markers must not be
// retried: retrying ordinary answers doubles the cost of every conversation.
func TestAnthropicNoRetryForPlainAnswer(t *testing.T) {
	up, calls := scriptedUpstream(t, frames("The answer is 42."))
	s := newTestServer(t, up.URL)
	doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", anthropicToolRequest)
	if *calls != 1 {
		t.Errorf("upstream calls = %d, want 1", *calls)
	}
}

// A request declaring no tools must never trigger the retry path, whatever the
// reply looks like.
func TestAnthropicNoRetryWithoutTools(t *testing.T) {
	up, calls := scriptedUpstream(t,
		frames(`<tool_call><function=shell><parameter=cmd>ls</parameter>`),
		frames(`<tool_call><function=shell><parameter=cmd>ls</parameter></function></tool_call>`),
	)
	s := newTestServer(t, up.URL)
	doJSON(t, s.Routes(), http.MethodPost, "/v1/messages",
		`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if *calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (no tools declared)", *calls)
	}
}

// Several calls in one turn must each get their own block with a distinct id,
// or the client cannot pair results back to calls.
func TestAnthropicMultipleToolUseBlocks(t *testing.T) {
	reply := `<tool_call><function=shell><parameter=cmd>["ls"]</parameter></function></tool_call>` +
		`<tool_call><function=read_file><parameter=path>/etc/hosts</parameter></function></tool_call>`
	up := mockUpstream(t, []string{frames(reply)})
	s := newTestServer(t, up.URL)

	body := `{"model":"m","max_tokens":256,
	  "tools":[
	    {"name":"shell","description":"run","input_schema":{"type":"object"}},
	    {"name":"read_file","description":"read","input_schema":{"type":"object"}}],
	  "messages":[{"role":"user","content":"do both"}]}`
	rec := doJSON(t, s.Routes(), http.MethodPost, "/v1/messages", body)

	var resp struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var ids []string
	var names []string
	for _, b := range resp.Content {
		if b.Type == "tool_use" {
			ids = append(ids, b.ID)
			names = append(names, b.Name)
		}
	}
	if len(names) != 2 {
		t.Fatalf("got %d tool_use blocks (%v), want 2: %s", len(names), names, rec.Body.String())
	}
	if ids[0] == ids[1] || ids[0] == "" || ids[1] == "" {
		t.Errorf("ids are not distinct and non-empty: %v", ids)
	}
}
