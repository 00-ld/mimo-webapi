package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"mimowebapi/internal/upstream"
)

// ---- Responses API wire types ---------------------------------------------
//
// The Responses API is the newer OpenAI shape. Agents that speak it post here
// instead of /v1/chat/completions, and an upstream that only answers the older
// endpoint looks broken to them: they send one request, receive a 404, and
// report the upstream as unreachable.
//
// The two differences that matter for a relay are the request field names
// (input instead of messages, max_output_tokens instead of max_tokens,
// instructions instead of a system message) and the response envelope (a
// typed event stream and an output item list rather than choices and deltas).

// ResponsesRequest is the inbound request body.
//
// Every field is optional apart from a usable input, because clients differ
// in how much of the spec they populate and an unknown field must not be an
// error — clients add parameters faster than any relay tracks them.
type ResponsesRequest struct {
	Model  string          `json:"model"`
	Input  json.RawMessage `json:"input"`
	Stream bool            `json:"stream"`

	// Instructions is the system prompt in this API. A client may send it
	// alongside, or instead of, a system-role item inside input.
	Instructions string `json:"instructions,omitempty"`

	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	Reasoning       *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`

	// Extensions shared with the chat endpoint, so a client can use the same
	// config regardless of which API it speaks.
	WebSearch *bool `json:"web_search,omitempty"`
	// Thinking accepts both a bare boolean and an object; see
	// parseThinkingFlag for why it is not typed as bool.
	Thinking json.RawMessage `json:"thinking,omitempty"`

	// Tools are declared by the client and reconstructed by the relay, since
	// the upstream accepts only a plain-text query.
	Tools json.RawMessage `json:"tools,omitempty"`

	// previous_response_id is part of the spec but this relay is deliberately
	// stateless across requests: the upstream conversation id is derived from
	// the transcript, so replaying history is both simpler and more correct
	// than tracking server-side state that a restart would lose.
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

// inputItem is one entry of the input array.
type inputItem struct {
	Type    string          `json:"type,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// Shorthand form: an item may carry text directly.
	Text string `json:"text,omitempty"`

	// Function call items. An assistant turn that requested a tool is echoed
	// back as a function_call item and its result as function_call_output;
	// both carry what is needed to restate the exchange as prompt text.
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

// toChatMessages flattens the input field into the message list the turn
// builder already understands.
//
// input accepts three shapes in practice:
//
//	"a plain string"
//	[{"role":"user","content":"..."}]
//	[{"type":"message","role":"user","content":[{"type":"input_text","text":"..."}]}]
//
// All three must work, because clients pick different ones.
func (r *ResponsesRequest) toChatMessages() ([]ChatMessage, error) {
	raw := strings.TrimSpace(string(r.Input))
	if raw == "" || raw == "null" {
		// A request may carry only instructions, which is a valid system-only
		// prompt but leaves nothing to answer.
		if r.Instructions != "" {
			return nil, fmt.Errorf("input is required")
		}
		return nil, fmt.Errorf("input is required")
	}

	// Plain string form.
	var s string
	if err := json.Unmarshal(r.Input, &s); err == nil {
		msgs := make([]ChatMessage, 0, 2)
		if r.Instructions != "" {
			msgs = append(msgs, textMessage("system", r.Instructions))
		}
		if strings.TrimSpace(s) != "" {
			msgs = append(msgs, textMessage("user", s))
		}
		if len(msgs) == 0 {
			return nil, fmt.Errorf("input is empty")
		}
		return msgs, nil
	}

	var items []inputItem
	if err := json.Unmarshal(r.Input, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or an array of items")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("input is empty")
	}

	msgs := make([]ChatMessage, 0, len(items)+1)
	if r.Instructions != "" {
		msgs = append(msgs, textMessage("system", r.Instructions))
	}
	for _, it := range items {
		// Item types fall into two families: ones that carry conversation
		// prose ("message", and the shorthand where type is omitted) and ones
		// that carry protocol state. A reasoning or tool-call item may still
		// have a text-shaped content field, so the type has to be checked
		// before the content — otherwise a scratchpad ends up in the prompt as
		// if the user had typed it.
		switch it.Type {
		case "", "message", "input_text":
			// Conversational. Text is expected.
		case "function_call":
			// A previous request for a tool. Restating it is what makes the
			// following result interpretable.
			args := compactJSON(it.Arguments)
			if args == "" {
				args = "{}"
			}
			msgs = append(msgs, textMessage("assistant",
				fmt.Sprintf("[called tool %s(%s)]", it.Name, args)))
			continue
		case "function_call_output":
			// The result of a tool call, rendered as context. The call id is
			// included so the model can match it to the request when several
			// are in flight.
			out := extractRawText(it.Output)
			if strings.TrimSpace(out) == "" {
				continue
			}
			label := "tool result"
			if it.CallID != "" {
				label = "tool result for call " + it.CallID
			}
			msgs = append(msgs, textMessage("user", label+":\n"+out))
			continue
		default:
			continue
		}

		text := extractItemText(it)
		if strings.TrimSpace(text) == "" {
			// A message item with no prose contributes nothing. Dropping it is
			// right for a relay whose upstream takes plain text.
			continue
		}
		role := it.Role
		if role == "" {
			role = "user"
		}
		// The upstream only distinguishes system/user/assistant. Any other
		// role is treated as user input rather than dropped.
		switch role {
		case "system", "user", "assistant":
		default:
			role = "user"
		}
		msgs = append(msgs, textMessage(role, text))
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input contained no text")
	}
	return msgs, nil
}

// extractItemText pulls prose out of one input item, accepting both the
// content-array form and the direct text form.
func extractItemText(it inputItem) string {
	if it.Text != "" {
		return it.Text
	}
	if len(it.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(it.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(it.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			// input_text and output_text are the text-bearing part types. A
			// reasoning or image part has no text field to contribute.
			if p.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// extractRawText reads a field that may be either a plain string or a JSON
// value. A tool output is a string in the common case but a structured object
// when the tool returns data.
func extractRawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return compactJSON(raw)
}

func textMessage(role, text string) ChatMessage {
	raw, _ := json.Marshal(text)
	return ChatMessage{Role: role, Content: raw}
}

// ---- Response objects ------------------------------------------------------

// responseObject is the non-streaming response, and also the payload carried
// by the response.completed streaming event.
//
// Optional fields use pointers or omitempty so a client that validates
// strictly is not tripped by a null where it expects an object.
type responseObject struct {
	ID        string  `json:"id"`
	Object    string  `json:"object"`
	CreatedAt int64   `json:"created_at"`
	Status    string  `json:"status"`
	Model     string  `json:"model"`
	Output    []any   `json:"output"`
	Usage     *rUsage `json:"usage,omitempty"`

	// Error is present only on a failed response.
	Error *responseError `json:"error,omitempty"`

	// Convenience fields the spec includes and SDKs read.
	OutputText string `json:"output_text,omitempty"`

	ParallelToolCalls bool  `json:"parallel_tool_calls"`
	ToolChoice        any   `json:"tool_choice"`
	Tools             []any `json:"tools"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// rUsage mirrors the Responses token accounting, which names the fields
// differently from chat completions.
type rUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// outputMessage is the assistant message item inside output.
type outputMessage struct {
	Type    string           `json:"type"`
	ID      string           `json:"id"`
	Status  string           `json:"status"`
	Role    string           `json:"role"`
	Content []outputTextPart `json:"content"`
}

// outputFunctionCall is a tool request in the Responses item shape.
type outputFunctionCall struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

type outputTextPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

// newResponseObjectWithCalls builds a response whose output is one or more
// function calls rather than a message.
//
// An empty call list falls back to the message shape, so callers can pass the
// parsed calls unconditionally.
func newResponseObjectWithCalls(id, model, text string, usage *Usage, calls []ToolCall) *responseObject {
	if len(calls) == 0 {
		return newResponseObject(id, model, text, usage)
	}
	items := make([]any, 0, len(calls))
	for _, c := range calls {
		items = append(items, outputFunctionCall{
			Type:      "function_call",
			ID:        "fc_" + c.ID,
			CallID:    c.ID,
			Name:      c.Name,
			Arguments: c.Arguments,
			Status:    "completed",
		})
	}
	obj := &responseObject{
		ID:                id,
		Object:            "response",
		CreatedAt:         time.Now().Unix(),
		Status:            "completed",
		Model:             model,
		Output:            items,
		OutputText:        text,
		ParallelToolCalls: true,
		ToolChoice:        "auto",
		Tools:             []any{},
	}
	if usage != nil {
		obj.Usage = &rUsage{
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
			TotalTokens:  usage.TotalTokens,
		}
	}
	return obj
}

// newResponseObject builds the completed response around a block of text.
func newResponseObject(id, model, text string, usage *Usage) *responseObject {
	msgID := "msg_" + strings.TrimPrefix(id, "resp_")
	obj := &responseObject{
		ID:                id,
		Object:            "response",
		CreatedAt:         time.Now().Unix(),
		Status:            "completed",
		Model:             model,
		OutputText:        text,
		ParallelToolCalls: true,
		ToolChoice:        "auto",
		Tools:             []any{},
	}
	obj.Output = []any{outputMessage{
		Type:   "message",
		ID:     msgID,
		Status: "completed",
		Role:   "assistant",
		Content: []outputTextPart{{
			Type:        "output_text",
			Text:        text,
			Annotations: []any{},
		}},
	}}
	if usage != nil {
		obj.Usage = &rUsage{
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
			TotalTokens:  usage.TotalTokens,
		}
	}
	return obj
}

// newResponseID mints an id in the shape clients expect for this API.
func newResponseID() string { return newCompletionID("resp") }

// ---- Handlers --------------------------------------------------------------

// handleResponses serves POST /v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error",
			"method_not_allowed", "use POST")
		return
	}
	body, err := readBody(w, r, s.cfg.Upstream.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "bad_body", err.Error())
		return
	}
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}

	messages, err := req.toChatMessages()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", err.Error())
		return
	}

	history, finalQuery, err := toTurns(messages, s.cfg.Behavior.SystemPromptMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", err.Error())
		return
	}
	query := upstream.ComposeQuery(history, finalQuery, s.cfg.Behavior.SystemPromptMode)

	model := s.cfg.ResolveModel(req.Model)
	mc := upstream.ModelConfig{
		EnableThinking:  s.responsesThinkingEnabled(req),
		WebSearchStatus: s.responsesWebSearchStatus(req),
		Model:           model,
	}
	if req.Temperature != nil {
		mc.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		mc.TopP = *req.TopP
	}

	// Tool definitions are rendered into the prompt; the reply is parsed back
	// into function_call items afterwards.
	defs := parseToolDefs(req.Tools)
	if block := renderPromptBlock(defs); block != "" {
		query += block
	}

	convID := s.conversations.KeyFor(history)
	upReq := upstream.NewChatRequest(convID, model, mc, query, nil)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := s.client.Chat(ctx, upReq)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}

	id := newResponseID()
	if req.Stream {
		s.streamResponses(w, ctx, stream, id, model, defs,
			utf8.RuneCountInString(upReq.Query))
		return
	}
	s.collectResponsesWithRetry(w, ctx, stream, id, model, defs, upReq)
}

func (s *Server) responsesThinkingEnabled(req ResponsesRequest) bool {
	if on, ok := parseThinkingFlag(req.Thinking); ok {
		return on
	}
	effort := ""
	if req.Reasoning != nil {
		effort = req.Reasoning.Effort
	}
	switch strings.ToLower(effort) {
	case "none", "off", "disabled":
		return false
	case "":
		return s.cfg.Behavior.EnableThinkingDefault
	default:
		return true
	}
}

func (s *Server) responsesWebSearchStatus(req ResponsesRequest) string {
	if req.WebSearch != nil {
		if *req.WebSearch {
			return "enabled"
		}
		return "disabled"
	}
	if s.cfg.Behavior.WebSearchDefault == "" {
		return "auto"
	}
	return s.cfg.Behavior.WebSearchDefault
}

// collectResponsesWithRetry buffers the reply and retries once with a format
// correction when a tool call was attempted but could not be parsed.
func (s *Server) collectResponsesWithRetry(w http.ResponseWriter, ctx context.Context,
	stream *upstream.ChatStream, id, model string, defs []ToolDef,
	upReq upstream.ChatRequest) {

	raw, usage, _, drainErr := s.drainOpenAI(stream, upReq)
	if drainErr != nil {
		// A failure discovered mid-stream is classified exactly as it is on
		// every other path, so an oversized conversation reports
		// context_length_exceeded rather than a generic upstream failure.
		s.writeUpstreamError(w, drainErr)
		return
	}
	_, answer := splitThink(raw)
	calls, remaining := parseToolCalls(answer, defs)

	if len(defs) > 0 && len(calls) == 0 && looksLikeFailedCall(answer) {
		retryReq := upReq
		retryReq.Query += toolRetryInstruction
		if second, err := s.client.Chat(ctx, retryReq); err == nil {
			raw2, usage2, _, _ := s.drainOpenAI(second, retryReq)
			_, answer2 := splitThink(raw2)
			if calls2, remaining2 := parseToolCalls(answer2, defs); len(calls2) > 0 {
				usage = usage2
				remaining, calls = remaining2, calls2
			}
		}
	}

	s.recordUsage(ctx, usage)
	writeJSON(w, http.StatusOK,
		newResponseObjectWithCalls(id, model, remaining, usage, calls))
}

// streamResponses emits the typed event sequence this API requires.
//
// The order is fixed by the spec and clients rely on it: created, an
// in-progress marker, the message item opening, a delta per token, the text
// closing, the item closing, then completed carrying the whole response.
// Skipping the bookkeeping events leaves a client waiting for a terminal state
// that never arrives.
func (s *Server) streamResponses(w http.ResponseWriter, ctx context.Context,
	stream *upstream.ChatStream, id, model string, defs []ToolDef,
	queryChars int) {

	sw, err := newSSEWriter(w)
	if err != nil {
		stream.Close(err)
		return
	}

	msgID := "msg_" + strings.TrimPrefix(id, "resp_")
	empty := newResponseObject(id, model, "", nil)
	empty.Status = "in_progress"
	empty.OutputText = ""

	// created
	if err := sw.event("response.created", map[string]any{
		"type": "response.created", "response": empty,
	}); err != nil {
		stream.Close(err)
		return
	}
	// in_progress
	if err := sw.event("response.in_progress", map[string]any{
		"type": "response.in_progress", "response": empty,
	}); err != nil {
		stream.Close(err)
		return
	}
	// The message item opens before any text arrives.
	itemOpen := outputMessage{
		Type: "message", ID: msgID, Status: "in_progress",
		Role: "assistant", Content: []outputTextPart{},
	}
	if err := sw.event("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0, "item": itemOpen,
	}); err != nil {
		stream.Close(err)
		return
	}
	if err := sw.event("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": msgID,
		"output_index": 0, "content_index": 0,
		"part": outputTextPart{Type: "output_text", Text: "", Annotations: []any{}},
	}); err != nil {
		stream.Close(err)
		return
	}

	var text strings.Builder
	var usage *Usage
	var streamErr error
	sequence := 0
	strip := &thinkStripper{}
	toolParser := &toolStreamParser{defs: defs}

	for frame := range stream.Frames {
		switch frame.Event {
		case frameMessage:
			if frame.Content == "" {
				continue
			}
			// A marker can straddle two chunks, so the stripper holds back the
			// ambiguous tail instead of guessing.
			visible := strip.push(frame.Content)
			if visible == "" {
				continue
			}
			// Buffer until it is clear whether this reply is prose or a tool
			// call. A call must never be emitted as output text: the client
			// would render the envelope rather than run the tool.
			released := toolParser.observe(visible)
			if released == "" {
				continue
			}
			text.WriteString(released)
			sequence++
			if err := sw.event("response.output_text.delta", map[string]any{
				"type":            "response.output_text.delta",
				"item_id":         msgID,
				"output_index":    0,
				"content_index":   0,
				"delta":           released,
				"sequence_number": sequence,
			}); err != nil {
				stream.Close(err)
				return
			}
		case frameUsage:
			if frame.Usage != nil {
				usage = &Usage{
					PromptTokens:     frame.Usage.PromptTokens,
					CompletionTokens: frame.Usage.CompletionTokens,
					TotalTokens:      frame.Usage.TotalTokens,
				}
			}
		case frameError:
			streamErr = upstreamFrameError(frame.Content,
				queryChars, s.cfg.Upstream.MaxQueryChars)
		}
	}

	// A read failure means the reply is incomplete; it is reported through the
	// same error event as an upstream error frame so it cannot be mistaken for
	// a finished response.
	if err := stream.ReadError(); err != nil && streamErr == nil {
		streamErr = err
	}

	if streamErr != nil {
		stream.Close(streamErr)
		// The stream is already open and committed to 200, so the failure is
		// reported as an event rather than a status code. The code carries the
		// classification, so an oversized request is reported as
		// context_length_exceeded and the client can compact instead of
		// retrying the same payload.
		_, code, msg, _ := upstreamErrorShape(streamErr, s.cfg.Upstream.MaxQueryChars)
		s.log.Warn("responses stream finished with upstream error",
			"code", code, "error", streamErr)
		_ = sw.event("error", map[string]any{
			"type": "error",
			"error": responseError{
				Code:    code,
				Message: msg,
			},
		})
		return
	}
	stream.Close(nil)
	s.recordUsage(ctx, usage)

	if tail := strip.flush(); tail != "" {
		if released := toolParser.observe(tail); released != "" {
			text.WriteString(released)
			sequence++
			if err := sw.event("response.output_text.delta", map[string]any{
				"type":            "response.output_text.delta",
				"item_id":         msgID,
				"output_index":    0,
				"content_index":   0,
				"delta":           released,
				"sequence_number": sequence,
			}); err != nil {
				return
			}
		}
	}

	// Resolve the parser: anything buffered that is not a call is released as
	// text so the client still sees the model's answer.
	tailText, calls := toolParser.finish()
	if tailText != "" {
		text.WriteString(tailText)
		sequence++
		if err := sw.event("response.output_text.delta", map[string]any{
			"type":            "response.output_text.delta",
			"item_id":         msgID,
			"output_index":    0,
			"content_index":   0,
			"delta":           tailText,
			"sequence_number": sequence,
		}); err != nil {
			return
		}
	}

	final := text.String()

	if len(calls) > 0 {
		// A tool request is a different kind of output item, so the message
		// item opened at the start is closed empty and a function_call item
		// follows. The order matters: a client waiting on the item it was told
		// about needs that item closed before the next one opens.
		if err := sw.event("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": msgID,
			"output_index": 0, "content_index": 0, "text": final,
		}); err != nil {
			return
		}
		if err := sw.event("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": outputMessage{
				Type: "message", ID: msgID, Status: "completed", Role: "assistant",
				Content: []outputTextPart{},
			},
		}); err != nil {
			return
		}

		fcItems := make([]any, 0, len(calls))
		for i, c := range calls {
			idx := i + 1
			fcID := "fc_" + c.ID
			item := outputFunctionCall{
				Type: "function_call", ID: fcID, CallID: c.ID,
				Name: c.Name, Arguments: "", Status: "in_progress",
			}
			if err := sw.event("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": idx, "item": item,
			}); err != nil {
				return
			}
			sequence++
			if err := sw.event("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": fcID,
				"output_index": idx, "delta": c.Arguments, "sequence_number": sequence,
			}); err != nil {
				return
			}
			sequence++
			if err := sw.event("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "item_id": fcID,
				"output_index": idx, "arguments": c.Arguments, "sequence_number": sequence,
			}); err != nil {
				return
			}
			item.Arguments = c.Arguments
			item.Status = "completed"
			fcItems = append(fcItems, item)
			if err := sw.event("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": idx, "item": item,
			}); err != nil {
				return
			}
		}

		obj := newResponseObjectWithCalls(id, model, final, usage, calls)
		_ = sw.event("response.completed", map[string]any{
			"type": "response.completed", "response": obj,
		})
		return
	}

	if err := sw.event("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": msgID,
		"output_index": 0, "content_index": 0, "text": final,
	}); err != nil {
		return
	}
	if err := sw.event("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": msgID,
		"output_index": 0, "content_index": 0,
		"part": outputTextPart{Type: "output_text", Text: final, Annotations: []any{}},
	}); err != nil {
		return
	}
	if err := sw.event("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": newResponseObject(id, model, final, usage).Output[0],
	}); err != nil {
		return
	}
	_ = sw.event("response.completed", map[string]any{
		"type":     "response.completed",
		"response": newResponseObject(id, model, final, usage),
	})
}
