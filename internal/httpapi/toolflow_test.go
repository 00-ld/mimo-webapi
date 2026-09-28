package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"mimowebapi/internal/upstream"
)

// flattenTurns renders the history for assertions.
func flattenTurns(turns []upstream.Turn) string {
	var b strings.Builder
	for _, t := range turns {
		b.WriteString(t.Role)
		b.WriteString(": ")
		b.WriteString(t.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// Tool definitions arrive in two nestings. Both must normalise to the same
// thing, or a client using one protocol finds its tools missing.
func TestParseToolDefsBothNestings(t *testing.T) {
	chatForm := json.RawMessage(`[
		{"type":"function","function":{"name":"shell","description":"run","parameters":{"type":"object"}}}
	]`)
	respForm := json.RawMessage(`[
		{"type":"function","name":"shell","description":"run","parameters":{"type":"object"}}
	]`)

	a := parseToolDefs(chatForm)
	b := parseToolDefs(respForm)

	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("got %d and %d defs, want 1 each", len(a), len(b))
	}
	if a[0].Name != "shell" || b[0].Name != "shell" {
		t.Errorf("names = %q and %q", a[0].Name, b[0].Name)
	}
}

// An entry with no name cannot be rendered into the prompt and must not take
// the whole request down with it.
func TestParseToolDefsSkipsNameless(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"function","function":{"description":"no name here"}},
		{"type":"function","function":{"name":"shell","description":"run"}}
	]`)
	defs := parseToolDefs(raw)
	if len(defs) != 1 {
		t.Fatalf("got %d defs, want 1", len(defs))
	}
	if defs[0].Name != "shell" {
		t.Errorf("name = %q", defs[0].Name)
	}
}

// A duplicate name would be rendered twice and invite an arbitrary choice.
func TestParseToolDefsDropsDuplicates(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"function","function":{"name":"shell"}},
		{"type":"function","function":{"name":"shell"}}
	]`)
	if defs := parseToolDefs(raw); len(defs) != 1 {
		t.Errorf("got %d defs, want 1", len(defs))
	}
}

// A request with no tools must produce no prompt block at all, so an ordinary
// conversation is not burdened with tool instructions.
func TestRenderPromptBlockEmptyWithoutTools(t *testing.T) {
	if block := renderPromptBlock(nil); block != "" {
		t.Errorf("block = %q, want empty", block)
	}
}

// The instruction and the parser have to agree on the format. This asserts the
// contract string names the fields the parser looks for.
func TestPromptBlockDescribesTheParsedFormat(t *testing.T) {
	block := renderPromptBlock(testDefs)
	for _, want := range []string{"shell", "read_file", `"tool"`, `"arguments"`} {
		if !strings.Contains(block, want) {
			t.Errorf("prompt block is missing %q", want)
		}
	}
}

// Tool results reach the model only through the prompt, since the upstream has
// no tool role. Without this the model calls the tool again forever.
func TestToolResultsReachTheQuery(t *testing.T) {
	msgs := []ChatMessage{
		textMessage("user", "what is in /tmp"),
		{Role: "assistant", ToolCalls: json.RawMessage(
			`[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{}"}}]`)},
		{Role: "tool", ToolCallID: "call_1", Name: "shell",
			Content: json.RawMessage(`"alpha.txt\nbeta.log"`)},
	}
	history, finalQuery, err := toTurns(msgs, "auto")
	if err != nil {
		t.Fatalf("toTurns: %v", err)
	}
	whole := flattenTurns(history) + finalQuery
	for _, want := range []string{"alpha.txt", "beta.log", "call_1"} {
		if !strings.Contains(whole, want) {
			t.Errorf("query is missing %q:\n%s", want, whole)
		}
	}
}

// An assistant turn that requested a tool must leave a trace in the prompt, or
// the result that follows has nothing to attach to.
func TestAssistantToolCallIsRestated(t *testing.T) {
	msgs := []ChatMessage{
		textMessage("user", "run something"),
		{Role: "assistant", ToolCalls: json.RawMessage(
			`[{"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":[\"ls\"]}"}}]`)},
		{Role: "tool", ToolCallID: "c1", Name: "shell",
			Content: json.RawMessage(`"done"`)},
	}
	history, finalQuery, err := toTurns(msgs, "auto")
	if err != nil {
		t.Fatalf("toTurns: %v", err)
	}
	if !strings.Contains(flattenTurns(history)+finalQuery, "shell") {
		t.Errorf("the requested call is absent from the prompt:\nhistory=%v\nquery=%s",
			history, finalQuery)
	}
}

// A conversation of nothing but tool traffic must still produce a usable turn
// rather than the "no content" error.
func TestToolOnlyConversationIsValid(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "tool", ToolCallID: "c1", Name: "shell",
			Content: json.RawMessage(`"output"`)},
	}
	if _, _, err := toTurns(msgs, "auto"); err != nil {
		t.Errorf("toTurns rejected a tool-only conversation: %v", err)
	}
}

// The Responses input walker must render function call items instead of
// dropping them, or the second turn of every agent loop loses its context.
func TestResponsesFunctionCallItemsReachThePrompt(t *testing.T) {
	body := `{"model":"m","input":[
		{"role":"user","content":"list /tmp"},
		{"type":"function_call","call_id":"c9","name":"shell","arguments":"{\"cmd\":[\"ls\"]}"},
		{"type":"function_call_output","call_id":"c9","output":"alpha.txt"}
	]}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	msgs, err := req.toChatMessages()
	if err != nil {
		t.Fatalf("toChatMessages: %v", err)
	}
	history, finalQuery, err := toTurns(msgs, "auto")
	if err != nil {
		t.Fatalf("toTurns: %v", err)
	}
	whole := flattenTurns(history) + finalQuery
	for _, want := range []string{"shell", "alpha.txt", "c9"} {
		if !strings.Contains(whole, want) {
			t.Errorf("prompt is missing %q:\n%s", want, whole)
		}
	}
}

// A structured tool output (an object rather than a string) must still render.
func TestResponsesStructuredToolOutput(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"function_call_output","call_id":"c1","output":{"count":3,"ok":true}}
	]}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	msgs, err := req.toChatMessages()
	if err != nil {
		t.Fatalf("toChatMessages: %v", err)
	}
	history, finalQuery, err := toTurns(msgs, "auto")
	if err != nil {
		t.Fatalf("toTurns: %v", err)
	}
	whole := flattenTurns(history) + finalQuery
	if !strings.Contains(whole, "count") {
		t.Errorf("structured output was dropped:\n%s", whole)
	}
}

// A function_call output item must be shaped the way clients validate it.
func TestFunctionCallOutputItemShape(t *testing.T) {
	calls := []ToolCall{{ID: "call_1", Name: "shell", Arguments: `{"cmd":["ls"]}`}}
	obj := newResponseObjectWithCalls("resp_x", "m", "", nil, calls)

	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	item := back["output"].([]any)[0].(map[string]any)
	if item["type"] != "function_call" {
		t.Errorf("type = %v, want function_call", item["type"])
	}
	for _, k := range []string{"call_id", "name", "arguments"} {
		if _, ok := item[k]; !ok {
			t.Errorf("item is missing %q", k)
		}
	}
	if item["call_id"] != "call_1" {
		t.Errorf("call_id = %v, want the id the parser minted", item["call_id"])
	}
}

// With no calls the message shape must be used, so a client that only handles
// text is unaffected by the tool layer existing.
func TestNoCallsFallsBackToMessageShape(t *testing.T) {
	obj := newResponseObjectWithCalls("resp_x", "m", "hello", nil, nil)
	raw, _ := json.Marshal(obj)
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	item := back["output"].([]any)[0].(map[string]any)
	if item["type"] != "message" {
		t.Errorf("type = %v, want message", item["type"])
	}
}
