package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every shape a client actually sends must parse. Agents disagree about this
// field, and rejecting one of them makes the upstream look broken.
func TestResponsesInputForms(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "plain string",
			body: `{"model":"m","input":"hello"}`,
			want: "hello",
		},
		{
			name: "message with string content",
			body: `{"model":"m","input":[{"role":"user","content":"hello"}]}`,
			want: "hello",
		},
		{
			name: "message with content array",
			body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			want: "hello",
		},
		{
			name: "instructions become a system message",
			body: `{"model":"m","instructions":"be brief","input":"hello"}`,
			want: "hello",
		},
		{
			name: "multi-turn",
			body: `{"model":"m","input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`,
			want: "c",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req ResponsesRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			msgs, err := req.toChatMessages()
			if err != nil {
				t.Fatalf("toChatMessages: %v", err)
			}
			if len(msgs) == 0 {
				t.Fatal("no messages produced")
			}
			last := msgs[len(msgs)-1]
			if got := last.TextContent(); got != tc.want {
				t.Errorf("last message = %q, want %q", got, tc.want)
			}
			if req.Instructions != "" && msgs[0].Role != "system" {
				t.Errorf("instructions did not become a system message")
			}
		})
	}
}

// A request with nothing to answer is a client bug, and it must be reported as
// one rather than forwarded upstream.
func TestResponsesRejectsEmptyInput(t *testing.T) {
	for _, body := range []string{
		`{"model":"m"}`,
		`{"model":"m","input":""}`,
		`{"model":"m","input":[]}`,
		`{"model":"m","input":[{"type":"reasoning","content":"no text"}]}`,
	} {
		var req ResponsesRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if _, err := req.toChatMessages(); err == nil {
			t.Errorf("body %s produced no error, want one", body)
		}
	}
}

// Reasoning items carry no prose. Forwarding their JSON would corrupt the
// prompt, so they must be dropped rather than stringified.
func TestResponsesDropsNonTextItems(t *testing.T) {
	var req ResponsesRequest
	body := `{"model":"m","input":[{"type":"reasoning","content":"scratchpad"},{"role":"user","content":"real question"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	msgs, err := req.toChatMessages()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if got := msgs[0].TextContent(); got != "real question" {
		t.Errorf("message = %q, want %q", got, "real question")
	}
}

// The completed response must expose the text in both places clients look:
// the output item list, and the output_text convenience field.
func TestResponseObjectShape(t *testing.T) {
	obj := newResponseObject("resp_test", "m", "the answer", &Usage{
		PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13,
	})

	if obj.Object != "response" {
		t.Errorf("object = %q, want response", obj.Object)
	}
	if obj.Status != "completed" {
		t.Errorf("status = %q, want completed", obj.Status)
	}
	if obj.OutputText != "the answer" {
		t.Errorf("output_text = %q", obj.OutputText)
	}
	if len(obj.Output) != 1 {
		t.Fatalf("output has %d items, want 1", len(obj.Output))
	}

	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	items := back["output"].([]any)
	item := items[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Errorf("item = %v, want an assistant message", item)
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" {
		t.Errorf("content part type = %v, want output_text", content["type"])
	}
	if content["text"] != "the answer" {
		t.Errorf("content text = %v", content["text"])
	}

	usage := back["usage"].(map[string]any)
	for _, k := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if _, ok := usage[k]; !ok {
			t.Errorf("usage is missing %q", k)
		}
	}
}

// Reasoning must never reach output_text: this API has no field to carry it.
func TestResponseObjectExcludesReasoning(t *testing.T) {
	_, answer := splitThink("<think>scratch</think>visible")
	if strings.Contains(answer, "scratch") {
		t.Errorf("answer = %q, reasoning leaked in", answer)
	}
	obj := newResponseObject("resp_x", "m", answer, nil)
	if strings.Contains(obj.OutputText, "scratch") {
		t.Errorf("output_text = %q, reasoning leaked in", obj.OutputText)
	}
}
