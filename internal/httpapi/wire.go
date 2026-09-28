// Package httpapi implements the OpenAI- and Anthropic-compatible surfaces
// that sit in front of the MiMo Studio web backend.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mimowebapi/internal/util"
)

// ---- OpenAI wire types -----------------------------------------------------

// ChatMessage is one inbound message. Content may be a plain string or the
// multimodal array form; both are accepted.
type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name,omitempty"`

	// ToolCalls is present on an assistant message that requested tools, and
	// ToolCallID on the tool message that answers one. Both are needed to
	// rebuild the exchange for an upstream that has no concept of either.
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// TextContent flattens either content form into plain text.
func (m ChatMessage) TextContent() string {
	if len(m.Content) == 0 {
		return ""
	}
	// String form.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// Array form: [{type:"text",text:"..."}, ...]
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// ChatCompletionRequest is the inbound OpenAI request.
type ChatCompletionRequest struct {
	Model           string          `json:"model"`
	Messages        []ChatMessage   `json:"messages"`
	Stream          bool            `json:"stream"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxTokens       *int            `json:"max_tokens,omitempty"`
	MaxCompletion   *int            `json:"max_completion_tokens,omitempty"`
	Stop            json.RawMessage `json:"stop,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	// Tools are declared by the client and reconstructed by the relay, since
	// the upstream accepts only a plain-text query.
	Tools json.RawMessage `json:"tools,omitempty"`

	// Extensions understood by this relay. They are ignored by other
	// OpenAI-compatible servers, so a client can set them without breaking
	// portability.
	WebSearch *bool `json:"web_search,omitempty"`
	Thinking  *bool `json:"thinking,omitempty"`
}

// ChatCompletion is the non-streaming OpenAI response.
type ChatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice is one completion candidate.
type Choice struct {
	Index        int         `json:"index"`
	Message      *RespMsg    `json:"message,omitempty"`
	Delta        *RespMsg    `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason"`
	Logprobs     interface{} `json:"logprobs"`
}

// RespMsg carries the assistant text.
type RespMsg struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`

	// ToolCalls is non-empty when the model asked for tools instead of, or as
	// well as, answering.
	ToolCalls []ToolCallOut `json:"tool_calls,omitempty"`
}

// ToolCallOut is the OpenAI wire shape for one requested call.
type ToolCallOut struct {
	Index    *int            `json:"index,omitempty"`
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function ToolCallFuncOut `json:"function"`
}

// ToolCallFuncOut is the function half of a tool call.
type ToolCallFuncOut struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage mirrors OpenAI token accounting.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---- helpers ---------------------------------------------------------------

// writeJSON emits a JSON body with the status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"message":"encode failure"}}`,
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// APIError is the OpenAI-shaped error envelope.
type APIError struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner error object.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// writeError emits an OpenAI-shaped error.
//
// The HTTP status is chosen so that OpenAI SDKs surface it as a normal API
// error rather than a transport failure: auth problems are 401, upstream
// exhaustion is 502, malformed input is 400.
func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, APIError{ErrorBody{Message: msg, Type: typ, Code: code}})
}

// sseWriter owns framing of Server-Sent Events to the client.
//
// It flushes after every event so a client renders tokens as they arrive. The
// relay must never buffer a stream it is supposed to be forwarding.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("response writer does not support flushing")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w: w, flusher: f}, nil
}

// event writes one SSE event with a JSON payload.
func (s *sseWriter) event(name string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if name != "" {
		if _, err := io.WriteString(s.w, "event: "+name+"\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(s.w, "data: "); err != nil {
		return err
	}
	if _, err := s.w.Write(raw); err != nil {
		return err
	}
	if _, err := io.WriteString(s.w, "\n\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// heartbeat sends a comment line, which keeps intermediaries from closing an
// idle stream without confusing strict SSE parsers.
func (s *sseWriter) heartbeat() error {
	if _, err := io.WriteString(s.w, ": keep-alive\n\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// raw writes a pre-formatted SSE payload verbatim.
//
// It exists for the OpenAI `[DONE]` sentinel, which is deliberately not JSON:
// marshalling it would emit the quoted string "[DONE]" and clients would never
// see the terminator they wait for.
func (s *sseWriter) raw(payload string) error {
	if _, err := io.WriteString(s.w, "data: "+payload+"\n\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// newCompletionID returns an id shaped like OpenAI's.
func newCompletionID(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().Unix(), util.NewID()[:12])
}

// ptr returns a pointer to v, for the optional JSON fields.
func ptr[T any](v T) *T { return &v }
