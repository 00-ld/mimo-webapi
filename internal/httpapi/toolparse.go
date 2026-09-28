package httpapi

import (
	"encoding/json"
	"strings"
)

// Parsing tool calls out of free-form model output.
//
// The model was asked to reply with a single JSON object and nothing else. It
// will do that most of the time and will deviate the rest of the time, so the
// parser is built to recover a call from a reply that also contains prose,
// a code fence, or several candidate objects. A reply with no recognisable
// call is not an error — it is the model answering in prose, which is the
// correct behaviour for any turn that does not need a tool.

// toolCallEnvelope is the reply shape the prompt asks for.
type toolCallEnvelope struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	// Some models emit the OpenAI field names instead of the ones asked for.
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

// The model has its own native tool-call syntax, learned before this relay
// asked it for JSON, and it prefers that syntax regardless of instructions:
//
//	<tool_call><function=NAME><parameter=KEY>VALUE</parameter></function></tool_call>
//
// Prompting cannot reliably override it, so both formats are parsed. The XML
// form is tried first because it is the one the model actually reaches for.
var (
	xmlToolOpen  = "<tool_call>"
	xmlToolClose = "</tool_call>"
	xmlFuncOpen  = "<function="
	xmlParamOpen = "<parameter="
	xmlParamEnd  = "</parameter>"
)

// parseXMLToolCalls extracts calls written in the model's native syntax.
func parseXMLToolCalls(reply string, defs []ToolDef) ([]ToolCall, string) {
	if !strings.Contains(reply, xmlToolOpen) {
		return nil, reply
	}
	var calls []ToolCall
	rest := reply

	for {
		start := strings.Index(rest, xmlToolOpen)
		if start < 0 {
			break
		}
		end := strings.Index(rest[start:], xmlToolClose)
		if end < 0 {
			break
		}
		block := rest[start+len(xmlToolOpen) : start+end]
		// Everything outside the block is commentary.
		rest = rest[:start] + rest[start+end+len(xmlToolClose):]

		call, ok := parseXMLBlock(block, defs)
		if ok {
			calls = append(calls, call)
		}
	}
	return calls, rest
}

// parseXMLBlock decodes the inside of one <tool_call> element.
func parseXMLBlock(block string, defs []ToolDef) (ToolCall, bool) {
	fn := strings.Index(block, xmlFuncOpen)
	if fn < 0 {
		return ToolCall{}, false
	}
	nameEnd := strings.Index(block[fn:], ">")
	if nameEnd < 0 {
		return ToolCall{}, false
	}
	name := strings.TrimSpace(block[fn+len(xmlFuncOpen) : fn+nameEnd])
	if name == "" || !knownTool(name, defs) {
		return ToolCall{}, false
	}
	// Some models append a closing tag to the name.
	name = strings.TrimSuffix(name, "/")
	name = strings.TrimSpace(strings.TrimSuffix(name, "function"))
	name = canonicalToolName(name, defs)

	args := map[string]any{}
	body := block[fn+nameEnd:]
	for {
		p := indexParamOpen(body)
		if p < 0 {
			break
		}
		keyEnd := strings.Index(body[p:], ">")
		if keyEnd < 0 {
			break
		}
		key := decodeParamName(body[p+len(xmlParamOpen) : p+keyEnd])
		valueStart := p + keyEnd + 1
		valueEnd := strings.Index(body[valueStart:], xmlParamEnd)
		if valueEnd < 0 {
			break
		}
		raw := body[valueStart : valueStart+valueEnd]
		// The model puts each parameter on its own line, so the value is
		// surrounded by exactly one newline on either side. Trimming all
		// whitespace would be wrong for an argument whose leading or trailing
		// spaces are significant, and trimming nothing would make every patch
		// arrive with a stray newline. Stripping the single structural newline
		// is the narrow rule that satisfies both.
		raw = stripOneSurroundingNewline(raw)
		if key != "" {
			args[key] = decodeScalarOrJSON(raw)
		}
		body = body[valueStart+valueEnd+len(xmlParamEnd):]
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		return ToolCall{}, false
	}
	return ToolCall{ID: newCallID(), Name: name, Arguments: string(encoded)}, true
}

// decodeScalarOrJSON interprets a parameter value.
//
// The model writes arrays and objects as JSON and everything else as bare
// text, so a value is tried as JSON first and kept as a string otherwise.
// Without the fallback, a path like "src/main.go" would fail to parse and be
// dropped.
//
// The raw text is returned unescaped when it is not valid JSON, because that
// is the case that carries long prose arguments: a patch, a file body, a shell
// snippet. Those contain newlines, quotes and backslashes that must reach the
// client byte for byte.
func decodeScalarOrJSON(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		return v
	}
	return decodeXMLEntities(raw)
}

// xmlEntityReplacer decodes the entities a model emits when it wants literal
// angle brackets or ampersands inside a <parameter> value.
//
// The model HTML-escapes inconsistently — sometimes it escapes, sometimes it
// does not — so decoding is applied only to text that failed to parse as JSON,
// and only for the five predefined XML entities. Anything longer or more exotic
// is left alone: a patch that legitimately contains "&amp;" as source code for
// an XML file would otherwise be silently corrupted, which is worse than
// leaving an entity undecoded.
//
// &amp; is replaced last. Doing it first would turn "&amp;lt;" into "&lt;" and
// then into "<", decoding two levels where the model escaped one.
var xmlEntityReplacer = strings.NewReplacer(
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&apos;", "'",
	"&#39;", "'",
)

// decodeXMLEntities reverses XML escaping in a parameter value.
//
// &amp; is handled by a final pass rather than by the replacer so that the
// other four are not applied to text that was themselves escaped as entities.
func decodeXMLEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	// A sentinel keeps "&amp;amp;" from collapsing twice: the ampersand form is
	// parked, the other entities are decoded, then the parked form is restored
	// as a literal ampersand.
	const sentinel = "\x00AMP\x00"
	out := strings.ReplaceAll(s, "&amp;", sentinel)
	out = xmlEntityReplacer.Replace(out)
	out = strings.ReplaceAll(out, "&#38;", "&")
	if strings.Contains(out, sentinel) {
		out = strings.ReplaceAll(out, sentinel, "&")
		return out
	}
	return out
}

// decodeParamName unescapes a parameter name. Model output occasionally escapes
// the key as well as the value, and a key that arrives as "file_path" spelled
// with entities would never match the schema.
func decodeParamName(key string) string {
	return decodeXMLEntities(strings.TrimSpace(key))
}

// stripOneSurroundingNewline removes at most one structural newline from each
// end of a parameter value.
//
// The model's formatting puts the value on its own line, so the newlines
// adjacent to the tags are layout, not data. Interior newlines — the entire
// body of a patch — are left untouched. A value that is genuinely only
// whitespace collapses to empty, which is the correct reading of an empty
// argument.
func stripOneSurroundingNewline(s string) string {
	s = strings.TrimPrefix(s, "\r\n")
	s = strings.TrimPrefix(s, "\n")
	s = strings.TrimSuffix(s, "\r\n")
	s = strings.TrimSuffix(s, "\n")
	return s
}

// indexParamOpen finds the next <parameter= tag.
//
// It searches for the tag followed by a name character rather than the bare
// prefix, so a patch value that legitimately contains the literal text
// "<parameter=" as source code does not get mistaken for the start of the next
// parameter. That is the failure mode that silently truncates a patch.
func indexParamOpen(body string) int {
	for from := 0; from < len(body); {
		p := strings.Index(body[from:], xmlParamOpen)
		if p < 0 {
			return -1
		}
		p += from
		after := p + len(xmlParamOpen)
		if after < len(body) {
			c := body[after]
			// A parameter name starts immediately; anything else means this is
			// the prefix appearing as data.
			if c != '>' && c != '<' && !isSpaceByte(c) {
				return p
			}
		}
		from = p + len(xmlParamOpen)
	}
	return -1
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// parseToolCalls extracts zero or more tool calls from a model reply.
//
// It returns the calls plus the text that remains once the call markup is
// removed. A reply that is pure prose returns no calls and the whole reply as
// text, which is how an ordinary answer is distinguished from a call.
func parseToolCalls(reply string, defs []ToolDef) ([]ToolCall, string) {
	trimmed := strings.TrimSpace(reply)
	if trimmed == "" {
		return nil, ""
	}

	// The model's native syntax is checked first: when it reaches for a tool it
	// overwhelmingly uses this form, whatever the prompt asked for.
	if calls, rest := parseXMLToolCalls(trimmed, defs); len(calls) > 0 {
		return calls, strings.TrimSpace(stripFences(rest))
	}

	// Fast path: the whole reply is one JSON object.
	if call, ok := parseEnvelope(trimmed, defs); ok {
		return []ToolCall{call}, ""
	}

	// The reply may be fenced, or wrapped in prose. Scan for balanced JSON
	// objects and keep the ones that look like calls.
	candidates := scanJSONObjects(trimmed)
	calls := make([]ToolCall, 0, len(candidates))
	consumed := make([]string, 0, len(candidates))
	for _, c := range candidates {
		call, ok := parseEnvelope(c, defs)
		if !ok {
			continue
		}
		calls = append(calls, call)
		consumed = append(consumed, c)
	}
	if len(calls) == 0 {
		return nil, reply
	}

	// Whatever is left after removing the call JSON is the model's commentary.
	rest := trimmed
	for _, c := range consumed {
		rest = strings.Replace(rest, c, "", 1)
	}
	rest = stripFences(rest)
	return calls, strings.TrimSpace(rest)
}

// parseEnvelope decodes one candidate object as a tool call.
//
// A candidate is accepted only when it names a tool the request actually
// declared. That check is what keeps an unrelated JSON object — a config
// snippet the model is discussing, say — from being mistaken for a call.
func parseEnvelope(s string, defs []ToolDef) (ToolCall, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s[0] != '{' {
		return ToolCall{}, false
	}
	var env toolCallEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return ToolCall{}, false
	}

	name := env.Tool
	if name == "" {
		name = env.Name
	}
	if name == "" {
		return ToolCall{}, false
	}
	if !knownTool(name, defs) {
		return ToolCall{}, false
	}

	raw := env.Arguments
	if len(raw) == 0 {
		raw = env.Args
	}
	args, ok := normaliseArguments(raw)
	if !ok {
		return ToolCall{}, false
	}

	// Return the declared spelling, not the model's, so a client matching on
	// the name it sent always finds it.
	return ToolCall{ID: newCallID(), Name: canonicalToolName(name, defs), Arguments: args}, true
}

// normaliseArguments renders the arguments as a compact JSON object string.
//
// Models produce three variants and all three have to be accepted:
//
//	{"path":"a"}      an object, which is the contract
//	"{\"path\":\"a\"}" a string containing JSON, a common deviation
//	(nothing)         a no-argument tool, which is valid for tools like a
//	                  status check with no parameters
func normaliseArguments(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "{}", true
	}

	// The contract: an object.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		out, err := json.Marshal(obj)
		if err != nil {
			return "{}", true
		}
		return string(out), true
	}

	// The deviation: a string that contains JSON.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return "{}", true
		}
		var inner map[string]any
		if err := json.Unmarshal([]byte(s), &inner); err == nil {
			out, err := json.Marshal(inner)
			if err != nil {
				return "{}", true
			}
			return string(out), true
		}
		// A string that is not an object: pass it through as a single
		// argument rather than dropping the call, so the tool still runs.
		out, err := json.Marshal(map[string]any{"input": s})
		if err != nil {
			return "{}", true
		}
		return string(out), true
	}

	return "{}", true
}

// knownTool reports whether the name was declared in this request.
//
// Matching is exact first, then case-insensitive, because a model that
// capitalises a tool name is making a spelling mistake rather than calling
// something else.
func knownTool(name string, defs []ToolDef) bool {
	if len(defs) == 0 {
		return false
	}
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	for _, d := range defs {
		if strings.EqualFold(d.Name, name) {
			return true
		}
	}
	return false
}

// canonicalToolName maps a model's spelling back to the declared name.
func canonicalToolName(name string, defs []ToolDef) string {
	for _, d := range defs {
		if d.Name == name {
			return d.Name
		}
	}
	for _, d := range defs {
		if strings.EqualFold(d.Name, name) {
			return d.Name
		}
	}
	return name
}

// scanJSONObjects finds top-level balanced {...} substrings.
//
// Brace counting is done rather than a JSON parser because the reply often has
// prose around the object and no parser will accept that as input. Braces
// inside strings are skipped, which matters for patches and code snippets.
func scanJSONObjects(s string) []string {
	var found []string
	depth := 0
	start := -1
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]

		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					found = append(found, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return found
}

// stripFences removes markdown code fences from leftover text.
func stripFences(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// ---- Streaming ------------------------------------------------------------

// toolStreamParser watches a token stream for a tool call.
//
// A tool call must be emitted as structured output, not as text, so the parser
// has to decide whether the reply is a call before any of it reaches the
// client. It buffers until that decision can be made: the first non-whitespace
// character settles it, because a call always begins with '{'.
type toolStreamParser struct {
	defs []ToolDef
	buf  strings.Builder
	// decided is true once the reply's nature is known.
	decided bool
	// isCall is meaningful only when decided is true.
	isCall bool
	// released tracks how much of buf has already been sent as text.
	released int
}

// observe feeds one chunk and returns any text that is safe to emit now.
//
// While the decision is pending, nothing is released. Once it is known that
// the reply is prose, everything buffered is released and subsequent chunks
// pass straight through.
func (p *toolStreamParser) observe(chunk string) string {
	p.buf.WriteString(chunk)
	if p.decided && !p.isCall {
		out := p.buf.String()[p.released:]
		p.released = p.buf.Len()
		return out
	}
	if p.decided {
		return ""
	}

	// Decide on the first non-space character, but wait for a little input
	// first so a leading fence is not mistaken for prose.
	s := p.buf.String()
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if trimmed == "" {
		return ""
	}
	if len(trimmed) < 1 {
		return ""
	}
	if trimmed[0] == '{' || strings.HasPrefix(trimmed, "<tool_call>") {
		// A call. Hold everything until it is complete.
		p.decided = true
		p.isCall = true
		return ""
	}
	if strings.HasPrefix("<tool_call>", trimmed) {
		// Too short to tell yet, but it is the start of one.
		return ""
	}
	if trimmed[0] == '`' {
		// A fenced reply: strip the fence line and decide on what follows.
		if idx := strings.Index(trimmed, "\n"); idx >= 0 {
			after := strings.TrimLeft(trimmed[idx+1:], " \t\r\n")
			if after == "" {
				// The opening fence line arrived but not the body yet.
				return ""
			}
			if after[0] == '{' {
				p.decided = true
				p.isCall = true
				return ""
			}
		} else if len(trimmed) < len("```json")+1 {
			// Still inside the opening fence token; wait for more.
			return ""
		}
		// A fence around prose, or an unterminated fence line longer than any
		// language tag: treat the reply as prose.
		p.decided = true
		p.isCall = false
		out := p.buf.String()
		p.released = p.buf.Len()
		return out
	}
	// Prose. Release what has accumulated.
	p.decided = true
	p.isCall = false
	out := p.buf.String()
	p.released = p.buf.Len()
	return out
}

// finish resolves the parser at end of stream.
//
// It returns the text to release and any tool calls found. A buffered reply
// that was held as a possible call but turns out to be malformed is released
// as text rather than dropped, so the user still sees the model's answer.
func (p *toolStreamParser) finish() (text string, calls []ToolCall) {
	if !p.decided {
		// The whole reply was shorter than the decision threshold.
		p.decided = true
		p.isCall = false
		out := p.buf.String()
		p.released = p.buf.Len()
		return out, nil
	}
	if !p.isCall {
		out := p.buf.String()[p.released:]
		p.released = p.buf.Len()
		return out, nil
	}
	calls, rest := parseToolCalls(p.buf.String(), p.defs)
	if len(calls) == 0 {
		// Held as a call but is not one. Release it as text.
		return p.buf.String(), nil
	}
	return rest, calls
}

// newCallID mints a call id in the shape clients expect.
func newCallID() string { return newCompletionID("call") }
