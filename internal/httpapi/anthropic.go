package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"mimowebapi/internal/upstream"
)

// ---- Anthropic wire types --------------------------------------------------

// AnthropicRequest is the inbound /v1/messages body.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	// Thinking is Anthropic's reasoning switch: {"type":"enabled"} or
	// {"type":"disabled"}.
	Thinking json.RawMessage `json:"thinking,omitempty"`
	// Metadata carries user_id for abuse tracing; unused upstream.
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// Tools are declared by the client and reconstructed by the relay, because
	// the upstream behind it accepts only a plain-text query and has no notion
	// of a function call.
	Tools []AnthropicTool `json:"tools,omitempty"`
	// ToolChoice is accepted and ignored beyond the fact of tools being
	// declared. The upstream cannot be forced to call anything; the prompt
	// asks politely and the parser verifies the result.
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
}

// AnthropicMessage is one turn. Content is a string or a block array.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text flattens either content form.
func (m AnthropicMessage) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" || blk.Type == "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// systemText flattens the system field, which may be a string or blocks.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		}
		return b.String()
	}
	return ""
}

// ---- Anthropic responses ---------------------------------------------------

type anthUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthMessageResp struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Role         string    `json:"role"`
	Model        string    `json:"model"`
	Content      []anthOut `json:"content"`
	StopReason   string    `json:"stop_reason"`
	StopSequence *string   `json:"stop_sequence"`
	Usage        anthUsage `json:"usage"`
}

// anthOut is one content block in a response.
//
// A text block carries text; a tool_use block carries id, name and input. The
// two are distinguished by type and the unused fields are omitted, because a
// strict client decoding a text block into a struct without an input field
// rejects the whole message when an unexpected one appears.
type anthOut struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Tool use fields.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// anthText is retained for the streaming text-block start event.
type anthText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolUseBlocks renders parsed calls as Anthropic tool_use blocks.
//
// The arguments arrive as a JSON string because that is the shape both
// protocols share internally; they are decoded back into a JSON value here so
// the block carries an object rather than a string containing JSON. A client
// that receives a string where it expects an object fails to dispatch the
// tool, which is the failure this conversion exists to prevent.
func toolUseBlocks(calls []ToolCall) []anthOut {
	if len(calls) == 0 {
		return nil
	}
	out := make([]anthOut, 0, len(calls))
	for _, c := range calls {
		input := json.RawMessage(c.Arguments)
		if len(input) == 0 || !json.Valid(input) {
			// A malformed argument would make the whole message unparseable.
			// An empty object keeps the call dispatchable with no parameters.
			input = json.RawMessage("{}")
		}
		id := c.ID
		if id == "" {
			id = newCallID()
		}
		out = append(out, anthOut{
			Type: "tool_use", ID: id, Name: c.Name, Input: input,
		})
	}
	return out
}

// anthropicContent builds the content array for a reply.
//
// Anthropic requires at least one block, and a turn that called a tool often
// has no prose at all. In that case the array holds only the tool_use block;
// when there is neither text nor a call, a single empty text block is emitted
// rather than an empty array, which clients reject.
func anthropicContent(text string, calls []ToolCall) []anthOut {
	blocks := make([]anthOut, 0, 1+len(calls))
	if text != "" {
		blocks = append(blocks, anthOut{Type: "text", Text: text})
	}
	blocks = append(blocks, toolUseBlocks(calls)...)
	if len(blocks) == 0 {
		blocks = append(blocks, anthOut{Type: "text", Text: ""})
	}
	return blocks
}

// handleAnthropicMessages serves POST /v1/messages.
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed,
			"invalid_request_error", "use POST")
		return
	}
	body, err := readBody(w, r, s.cfg.Upstream.MaxBodyBytes)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"messages: at least one message is required")
		return
	}

	defs := parseAnthropicToolDefs(req.Tools)

	turns, finalQuery, err := anthropicTurns(req, s.cfg.Behavior.SystemPromptMode)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	query := upstream.ComposeQuery(turns, finalQuery, s.cfg.Behavior.SystemPromptMode)
	// The upstream has no tool field, so the definitions go into the prompt and
	// the reply is parsed back into tool_use blocks afterwards.
	if block := renderPromptBlock(defs); block != "" {
		query += block
	}

	model := s.cfg.ResolveModel(req.Model)
	mc := upstream.ModelConfig{
		EnableThinking:  s.thinkingFromAnthropic(req),
		WebSearchStatus: s.cfg.Behavior.WebSearchDefault,
		Model:           model,
	}
	if mc.WebSearchStatus == "" {
		mc.WebSearchStatus = "auto"
	}
	if req.Temperature != nil {
		mc.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		mc.TopP = *req.TopP
	}

	convID := s.conversations.KeyFor(turns)
	upReq := upstream.NewChatRequest(convID, model, mc, query, nil)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := s.client.Chat(ctx, upReq)
	if err != nil {
		s.writeAnthropicUpstreamError(w, err)
		return
	}

	id := newCompletionID("msg")
	if req.Stream {
		s.streamAnthropic(w, ctx, stream, id, model, defs,
			utf8.RuneCountInString(upReq.Query))
		return
	}
	// A non-streaming reply can be inspected before anything is sent, so a
	// malformed tool call is worth one corrected attempt.
	s.collectAnthropicWithRetry(ctx, w, stream, id, model, req, defs, upReq)
}

func (s *Server) thinkingFromAnthropic(req AnthropicRequest) bool {
	// Shares parseThinkingFlag with the OpenAI-compatible paths so all three
	// protocols agree on what "thinking" means. Anthropic's native form is
	// {"type":"enabled"|"disabled"}, which the shared parser handles.
	if on, ok := parseThinkingFlag(req.Thinking); ok {
		return on
	}
	return s.cfg.Behavior.EnableThinkingDefault
}

// streamAnthropic forwards frames as Anthropic streaming events.
func (s *Server) streamAnthropic(w http.ResponseWriter, ctx context.Context,
	stream *upstream.ChatStream, id, model string, defs []ToolDef,
	queryChars int) {

	sw, err := newSSEWriter(w)
	if err != nil {
		stream.Close(err)
		return
	}
	go func() {
		<-ctx.Done()
		stream.Close(ctx.Err())
	}()

	// The event order below is fixed by the Anthropic streaming contract:
	// message_start, then content_block_start, deltas, content_block_stop,
	// message_delta, message_stop. Clients such as Claude Code assert on it.
	_ = sw.event("message_start", map[string]any{
		"type": "message_start",
		"message": anthMessageResp{
			ID: id, Type: "message", Role: "assistant", Model: model,
			Content: []anthOut{}, StopReason: "", Usage: anthUsage{},
		},
	})
	_ = sw.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": anthText{Type: "text", Text: ""},
	})
	_ = sw.event("ping", map[string]any{"type": "ping"})

	var usage *Usage
	var streamErr error
	stopReason := "end_turn"
	strip := &thinkStripper{}
	toolParser := &toolStreamParser{defs: defs}
	// blockIndex tracks which content block is currently open. The text block
	// is index 0; tool_use blocks follow it one at a time.
	blockIndex := 0
	// textOpen is false once the text block has been closed, which happens as
	// soon as the first tool_use block opens.
	textOpen := true

	closeTextBlock := func() {
		if !textOpen {
			return
		}
		_ = sw.event("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": blockIndex,
		})
		textOpen = false
	}

	// emitCall opens a tool_use block, streams its input as one
	// input_json_delta, and closes it.
	//
	// The arguments are sent whole rather than sliced. Anthropic clients
	// concatenate the partial_json fragments and parse the result, so a single
	// complete fragment is valid input to that accumulator. Splitting a long
	// patch across chunks would add a failure mode — a client that parses each
	// fragment independently — for no benefit, since the whole call is already
	// in hand by the time it can be emitted.
	emitCall := func(c ToolCall, idx int) error {
		closeTextBlock()
		blockIndex = idx
		toolID := c.ID
		if toolID == "" {
			toolID = newCallID()
		}
		if err := sw.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": blockIndex,
			"content_block": map[string]any{
				"type": "tool_use", "id": toolID, "name": c.Name, "input": map[string]any{},
			},
		}); err != nil {
			return err
		}
		if err := sw.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": blockIndex,
			"delta": map[string]string{
				"type": "input_json_delta", "partial_json": c.Arguments,
			},
		}); err != nil {
			return err
		}
		return sw.event("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": blockIndex,
		})
	}

	for frame := range stream.Frames {
		switch frame.Event {
		case frameMessage:
			if frame.Content == "" {
				continue
			}
			// The upstream interleaves its scratchpad with the answer. This
			// protocol has a thinking block for reasoning and a text block for
			// the answer, so the reasoning is held back rather than merged.
			_, answer := strip.pushSplit(frame.Content)
			if answer == "" {
				continue
			}
			// The tool parser decides whether this reply is prose or a call.
			visible := toolParser.observe(answer)
			if visible == "" {
				continue
			}
			if err := sw.event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]string{
					"type": "text_delta", "text": visible,
				},
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
			stopReason = "end_turn"
		case frameSensitiveQuery, frameSensitiveTitle:
			stopReason = "refusal"
		case frameTipTruncate:
			stopReason = "max_tokens"
		}
	}

	// A transport failure is surfaced as an error event below rather than
	// being silently absorbed into a normal end_turn.
	if err := stream.ReadError(); err != nil && streamErr == nil {
		streamErr = err
	}

	// Release whatever the stripper held back in case it was the start of a
	// marker. Without this the last word of every reply is truncated.
	if tail := strip.flush(); tail != "" {
		if visible := toolParser.observe(tail); visible != "" {
			if err := sw.event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]string{
					"type": "text_delta", "text": visible,
				},
			}); err != nil {
				stream.Close(err)
				return
			}
		}
	}

	tailText, calls := toolParser.finish()
	if tailText != "" {
		if err := sw.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{
				"type": "text_delta", "text": tailText,
			},
		}); err != nil {
			stream.Close(err)
			return
		}
	}

	if len(calls) > 0 {
		closeTextBlock()
		stopReason = "tool_use"
		for i, c := range calls {
			if err := emitCall(c, i+1); err != nil {
				stream.Close(err)
				return
			}
		}
	} else {
		_ = sw.event("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": blockIndex,
		})
		textOpen = false
	}

	outUsage := anthUsage{}
	if usage != nil {
		outUsage.InputTokens = usage.PromptTokens
		outUsage.OutputTokens = usage.CompletionTokens
	}
	_ = sw.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": outUsage,
	})
	_ = sw.event("message_stop", map[string]any{"type": "message_stop"})
	s.recordUsage(ctx, usage)

	if streamErr != nil {
		// The Anthropic stream contract has its own error event, and it must be
		// emitted before message_stop: a client that only sees a stop_reason
		// has nothing to branch on and will silently render an empty reply.
		_, code, msg, _ := upstreamErrorShape(streamErr, s.cfg.Upstream.MaxQueryChars)
		s.log.Warn("anthropic stream finished with upstream error",
			"code", code, "error", streamErr)
		_ = sw.event("error", map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    anthErrorType(code),
				"message": msg,
			},
		})
		stream.Close(streamErr)
		return
	}
	stream.Close(nil)
}

// anthErrorType maps a relay error code onto the Anthropic error taxonomy.
//
// Anthropic clients validate the inner `error.type` against a fixed set, so an
// unknown value is a parse error rather than a useful message. The oversized
// case maps onto the request-shape error, which is the class those clients
// already treat as "the request itself is wrong".
func anthErrorType(code string) string {
	switch code {
	case contextTooLongCode:
		return "invalid_request_error"
	case "cookies_expired":
		return "authentication_error"
	case "no_session":
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// collectAnthropicWithRetry drains the stream into one Anthropic message, and
// retries once with a format correction when the model attempted a tool call
// but produced nothing the parser could read.
//
// The retry lives here rather than upstream of the handler because only the
// non-streaming path may hold a reply back: a streaming client has already
// rendered the tokens by the time a malformed call is detectable.
func (s *Server) collectAnthropicWithRetry(ctx context.Context, w http.ResponseWriter,
	stream *upstream.ChatStream, id, model string, req AnthropicRequest,
	defs []ToolDef, upReq upstream.ChatRequest) {

	raw, usage, stopReason, streamErr := s.drainAnthropic(stream, upReq)
	if streamErr != nil {
		s.writeAnthropicUpstreamError(w, streamErr)
		return
	}

	text, calls, remaining := anthropicReply(raw, defs)

	if len(defs) > 0 && len(calls) == 0 && looksLikeFailedCall(text) {
		// Resend with the format restated. The original reply is discarded
		// rather than shown, because it is a failed attempt at a tool call and
		// presenting it as an answer is what makes an agent stall silently.
		retryReq := upReq
		retryReq.Query += toolRetryInstruction
		if second, err := s.client.Chat(ctx, retryReq); err == nil {
			raw2, usage2, stop2, err2 := s.drainAnthropic(second, retryReq)
			if err2 == nil {
				text2, calls2, remaining2 := anthropicReply(raw2, defs)
				if len(calls2) > 0 {
					raw, usage, stopReason = raw2, usage2, stop2
					text, calls, remaining = text2, calls2, remaining2
				}
			}
		}
	}

	out := anthMessageResp{
		ID: id, Type: "message", Role: "assistant", Model: model,
		Content:    anthropicContent(text, calls),
		StopReason: stopReason,
		Usage:      anthUsage{},
	}
	if len(calls) > 0 {
		out.StopReason = "tool_use"
		out.Content = anthropicContent(remaining, calls)
	}
	if usage != nil {
		out.Usage.InputTokens = usage.PromptTokens
		out.Usage.OutputTokens = usage.CompletionTokens
	} else {
		out.Usage.InputTokens = approxTokens(req.Model)
		out.Usage.OutputTokens = approxTokens(raw)
	}
	writeJSON(w, http.StatusOK, out)
}

// anthropicReply splits a raw upstream reply into visible text and calls.
//
// The reasoning scratchpad is removed first, because a call written inside it
// is the model thinking aloud rather than acting, and then the tool parser is
// run over what remains.
func anthropicReply(raw string, defs []ToolDef) (text string, calls []ToolCall, remaining string) {
	_, answer := splitThink(raw)
	calls, remaining = parseToolCalls(answer, defs)
	if len(calls) > 0 {
		return remaining, calls, remaining
	}
	return answer, nil, answer
}

// drainAnthropic consumes a stream into its raw text and metadata.
func (s *Server) drainAnthropic(stream *upstream.ChatStream,
	upReq upstream.ChatRequest) (string, *Usage, string, error) {
	var raw strings.Builder
	var usage *Usage
	var streamErr error
	stopReason := "end_turn"
	queryChars := utf8.RuneCountInString(upReq.Query)

	for frame := range stream.Frames {
		switch frame.Event {
		case frameMessage:
			raw.WriteString(frame.Content)
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
		case frameSensitiveQuery, frameSensitiveTitle:
			stopReason = "refusal"
		case frameTipTruncate:
			stopReason = "max_tokens"
		}
	}
	// A truncated transport must not be reported as a completed turn.
	if err := stream.ReadError(); err != nil && streamErr == nil {
		streamErr = err
	}
	stream.Close(streamErr)
	return raw.String(), usage, stopReason, streamErr
}

// collectAnthropic drains the stream into one Anthropic message.
//
// It takes the originating upstream request so an in-stream failure can be
// classified against the same query length the request was built with.
func (s *Server) collectAnthropic(ctx context.Context, w http.ResponseWriter,
	stream *upstream.ChatStream, id, model string, req AnthropicRequest, defs []ToolDef,
	upReq upstream.ChatRequest) {

	raw, usage, stopReason, streamErr := s.drainAnthropic(stream, upReq)
	if streamErr != nil {
		s.writeAnthropicUpstreamError(w, streamErr)
		return
	}
	s.recordUsage(ctx, usage)

	text, calls, remaining := anthropicReply(raw, defs)
	if len(calls) > 0 {
		stopReason = "tool_use"
		text = remaining
	}

	out := anthMessageResp{
		ID: id, Type: "message", Role: "assistant", Model: model,
		Content:    anthropicContent(text, calls),
		StopReason: stopReason,
		Usage:      anthUsage{},
	}
	if usage != nil {
		out.Usage.InputTokens = usage.PromptTokens
		out.Usage.OutputTokens = usage.CompletionTokens
	} else {
		out.Usage.InputTokens = approxTokens(req.Model)
		out.Usage.OutputTokens = approxTokens(raw)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) writeAnthropicUpstreamError(w http.ResponseWriter, err error) {
	status, code, msg, _ := upstreamErrorShape(err, s.cfg.Upstream.MaxQueryChars)
	writeAnthropicError(w, status, anthErrorType(code), msg)
}

// writeAnthropicError emits the Anthropic error envelope.
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}

// anthropicTurns normalizes an Anthropic request into upstream turns.
//
// Content is walked block by block rather than flattened to text, because the
// two block kinds that carry the tool protocol — tool_use on an assistant turn
// and tool_result on a user turn — have no text field and would otherwise
// vanish. Losing them breaks the agent loop in the worst way: the model never
// learns what its own call returned and simply issues it again.
func anthropicTurns(req AnthropicRequest, systemMode string) ([]upstream.Turn, string, error) {
	var turns []upstream.Turn
	if sys := strings.TrimSpace(systemText(req.System)); sys != "" {
		turns = append(turns, upstream.Turn{Role: "system", Content: sys})
	}

	var results []ToolResult
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "assistant" {
			role = "user"
		}
		blocks := anthropicBlocks(m.Content)

		var textParts []string
		var callParts []string
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				if b.Name == "" {
					continue
				}
				callParts = append(callParts, b.Name+"("+toolInputString(b.Input)+")")
			case "tool_result":
				content := b.ToolResultText()
				if strings.TrimSpace(content) == "" {
					continue
				}
				if b.IsError {
					// The failure has to be visible to the model, or it treats
					// an empty result as success and proceeds.
					content = "[tool error] " + content
				}
				results = append(results, ToolResult{
					CallID:  b.ToolResultID,
					Name:    anthropicResultName(b.ToolResultID, req),
					Content: content,
				})
			default:
				if strings.TrimSpace(b.Text) != "" {
					textParts = append(textParts, b.Text)
				}
			}
		}

		text := strings.TrimSpace(strings.Join(textParts, "\n"))
		if len(callParts) > 0 {
			// An assistant turn that called a tool often carries no text at
			// all. Recording what was asked for is what lets the following
			// result make sense to a model that has no memory of it.
			requested := "[called tools: " + strings.Join(callParts, "; ") + "]"
			if text == "" {
				text = requested
			} else {
				text += "\n" + requested
			}
		}
		if text == "" {
			continue
		}
		turns = append(turns, upstream.Turn{Role: role, Content: text})
	}

	if len(turns) == 0 && len(results) == 0 {
		return nil, "", fmt.Errorf("no message content")
	}
	if len(turns) == 0 {
		// A conversation made only of tool results. BuildQuery needs a final
		// turn to prompt on, and the results themselves are the prompt here:
		// an agent resuming after a tool ran has nothing else to say.
		turns = append(turns, upstream.Turn{Role: "user", Content: "Continue."})
	}

	history, finalQuery, err := upstream.BuildQuery(turns, systemMode)
	if err != nil {
		return nil, "", err
	}
	// Tool results have nowhere to go in the web protocol but the prompt.
	if block := renderToolResults(results); block != "" {
		finalQuery += block
	}
	return history, finalQuery, nil
}

// anthropicResultName recovers the tool name for a tool_result.
//
// The block carries only tool_use_id, so the name is looked up in the
// preceding assistant turns. The name matters because the rendered result is
// otherwise unattributable when a turn issued several calls, and it is not
// fatal when it cannot be found: the id is still rendered.
func anthropicResultName(toolUseID string, req AnthropicRequest) string {
	if toolUseID == "" {
		return ""
	}
	for _, m := range req.Messages {
		for _, b := range anthropicBlocks(m.Content) {
			if b.Type == "tool_use" && b.ToolUseID == toolUseID {
				return b.Name
			}
		}
	}
	return ""
}
