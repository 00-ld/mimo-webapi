package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Tool calling over a text-only upstream.
//
// The upstream behind this relay accepts a single plain-text query and returns
// plain text. It has no notion of tools, so tool calls cannot be passed
// through — they have to be reconstructed:
//
//	outbound: tool definitions are rendered into the prompt as instructions
//	          describing the available functions and the exact reply format
//	inbound:  the model's reply is parsed back into tool_calls
//
// The consequence is that the relay, not the upstream, is what guarantees the
// tool_call contract. Everything in this file exists to make that contract
// hold against a model that is only being asked politely to follow it.

// ---- Normalised tool definitions -------------------------------------------

// ToolDef is one callable function, in the shape both protocols agree on.
//
// The two inbound formats differ in nesting (chat completions wraps the
// function in {"type":"function","function":{...}}; the Responses API flattens
// it to {"type":"function","name":...,"parameters":...}), so both are decoded
// into this one type and nothing downstream has to care which arrived.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// toolDefFromChat decodes the chat completions nesting.
func toolDefFromChat(raw json.RawMessage) (*ToolDef, error) {
	var wrapper struct {
		Type     string `json:"type"`
		Function *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
		// The Responses shape may also appear here, since some clients send it
		// to the chat endpoint by mistake.
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	if wrapper.Function != nil && wrapper.Function.Name != "" {
		return &ToolDef{
			Name:        wrapper.Function.Name,
			Description: wrapper.Function.Description,
			Parameters:  wrapper.Function.Parameters,
		}, nil
	}
	if wrapper.Name != "" {
		return &ToolDef{
			Name:        wrapper.Name,
			Description: wrapper.Description,
			Parameters:  wrapper.Parameters,
		}, nil
	}
	return nil, fmt.Errorf("tool definition has no name")
}

// parseAnthropicToolDefs decodes Anthropic's tool shape.
//
// Anthropic names the schema field input_schema where OpenAI names it
// parameters; the two are otherwise identical. Keeping the translation in one
// place is what lets both protocols share the whole downstream pipeline —
// rendering, parsing and retrying are protocol-agnostic once a ToolDef exists.
func parseAnthropicToolDefs(tools []AnthropicTool) []ToolDef {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]ToolDef, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t.Name == "" || seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		defs = append(defs, ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	return defs
}

// parseToolDefs decodes a tools array in either nesting, dropping entries that
// carry no name rather than failing the whole request: one malformed tool is
// not a reason to refuse a conversation.
func parseToolDefs(raw json.RawMessage) []ToolDef {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	defs := make([]ToolDef, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		def, err := toolDefFromChat(it)
		if err != nil || def.Name == "" {
			continue
		}
		if seen[def.Name] {
			// A duplicate name would render twice in the prompt and invite the
			// model to pick arbitrarily.
			continue
		}
		seen[def.Name] = true
		defs = append(defs, *def)
	}
	return defs
}

// ---- Normalised tool calls -------------------------------------------------

// ToolCall is one function invocation produced by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// renderPromptBlock renders the tool definitions and the reply contract.
//
// The model is asked for one JSON object per line and nothing else. Prose
// around the call is unavoidable in practice, so the parser tolerates it; the
// instruction is worded strictly anyway because a model that stays inside the
// format produces far fewer ambiguous replies.
func renderPromptBlock(defs []ToolDef) string {
	if len(defs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Tools\n\n")
	b.WriteString("You may call the following tools. Call a tool when it is needed to ")
	b.WriteString("answer; otherwise reply normally in prose.\n\n")

	for _, d := range defs {
		b.WriteString("## ")
		b.WriteString(d.Name)
		b.WriteString("\n")
		if d.Description != "" {
			b.WriteString(d.Description)
			b.WriteString("\n")
		}
		if len(d.Parameters) > 0 {
			b.WriteString("Parameters (JSON Schema): ")
			b.WriteString(compactJSON(d.Parameters))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(replyContract)
	return b.String()
}

// replyContract is the exact output format the parser expects. It is kept in
// one place because the parser and the instruction have to stay in sync.
const replyContract = `To call a tool, reply with ONLY a single JSON object on one line, and nothing else:

{"tool": "<tool name>", "arguments": {<arguments as a JSON object>}}

Rules:
- One tool call per reply. To call several tools, wait for each result first.
- "arguments" must be a JSON object, never a string containing JSON.
- Do not wrap the JSON in a code fence and do not add commentary around it.
- When no tool is needed, reply in plain prose with no JSON.

`

// compactJSON renders raw JSON on one line so the prompt stays compact and the
// model does not imitate pretty-printed output.
func compactJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// appendToolResults folds tool results back into the prompt.
//
// The upstream has no tool role, so a result is rendered as context the model
// can read. The call_id is included because a model that issued several calls
// needs to know which result answers which call.
func renderToolResults(results []ToolResult) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Tool results\n\n")
	for _, r := range results {
		b.WriteString("## ")
		b.WriteString(r.Name)
		if r.CallID != "" {
			b.WriteString(" (call ")
			b.WriteString(r.CallID)
			b.WriteString(")")
		}
		b.WriteString("\n")
		b.WriteString(r.Content)
		b.WriteString("\n\n")
	}
	b.WriteString("Use these results to continue. Call another tool if needed, ")
	b.WriteString("otherwise give the final answer.\n")
	return b.String()
}

// ToolResult is one tool output returned by the client.
type ToolResult struct {
	CallID  string
	Name    string
	Content string
}

// ---- Anthropic content blocks ----------------------------------------------

// AnthropicTool is one declared tool in the Anthropic request shape.
//
// The schema field is input_schema rather than parameters; everything else
// lines up with ToolDef.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// AnthropicContentBlock is one element of a message's content array.
//
// All four block kinds an agent loop actually sends are represented, because
// dropping any of them breaks the conversation: a text block carries the
// prompt, a tool_use block records what the assistant asked for, and a
// tool_result block carries the output that has to reach the model.
type AnthropicContentBlock struct {
	Type string `json:"type"`
	// Text is set for a text block.
	Text string `json:"text,omitempty"`
	// ToolUseID, Name and Input are set for a tool_use block.
	ToolUseID string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	// ToolResultID names the call this block answers, for a tool_result block.
	ToolResultID string `json:"tool_use_id,omitempty"`
	// ToolResultContent is a string or a block array; raw so either is kept.
	ToolResultContent json.RawMessage `json:"content,omitempty"`
	// IsError marks a failed tool result. The upstream has no equivalent, so
	// it is rendered into the text instead of being dropped.
	IsError bool `json:"is_error,omitempty"`
}

// toolResultText flattens a tool_result's content, which is either a plain
// string or an array of blocks.
func (b AnthropicContentBlock) ToolResultText() string {
	if len(b.ToolResultContent) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(b.ToolResultContent, &s); err == nil {
		return s
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(b.ToolResultContent, &blocks); err == nil {
		var sb strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" || blk.Type == "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(blk.Text)
			}
		}
		return sb.String()
	}
	// A structured result (an object) is kept verbatim rather than discarded.
	return string(b.ToolResultContent)
}

// toolInputString renders a tool_use block's input as the compact JSON string
// the shared parser produces, so both protocols hand clients the same shape.
func toolInputString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Not valid JSON: the model produced something malformed. Wrapping it
		// keeps the argument reachable instead of losing the call.
		out, mErr := json.Marshal(map[string]any{"input": string(raw)})
		if mErr != nil {
			return "{}"
		}
		return string(out)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	// A non-object input is a deviation from the contract. Wrapping it in an
	// object means a strict client decoding into a map still succeeds.
	if _, ok := v.(map[string]any); !ok {
		wrapped, wErr := json.Marshal(map[string]any{"input": v})
		if wErr != nil {
			return string(out)
		}
		return string(wrapped)
	}
	return string(out)
}

// anthropicBlocks decodes a message's content into blocks.
//
// A plain string is normalised to a single text block so callers never have to
// branch on which form arrived.
func anthropicBlocks(raw json.RawMessage) []AnthropicContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []AnthropicContentBlock{{Type: "text", Text: s}}
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// anthropicToolUseBlocks extracts the tool_use blocks from an assistant turn.
//
// The id and name are kept because a following tool_result refers back to them
// by id, and without the pairing the result is unattributable when several
// calls were issued in one turn.
func anthropicToolUseBlocks(blocks []AnthropicContentBlock) []ToolCall {
	var calls []ToolCall
	for _, b := range blocks {
		if b.Type != "tool_use" || b.Name == "" {
			continue
		}
		calls = append(calls, ToolCall{
			ID:        b.ToolUseID,
			Name:      b.Name,
			Arguments: toolInputString(b.Input),
		})
	}
	return calls
}
