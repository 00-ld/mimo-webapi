package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

var testDefs = []ToolDef{
	{Name: "shell", Description: "run a command"},
	{Name: "read_file", Description: "read a file"},
}

func TestParseCleanCall(t *testing.T) {
	calls, text := parseToolCalls(
		`{"tool":"shell","arguments":{"cmd":["ls","-la"]}}`, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Name != "shell" {
		t.Errorf("name = %q", calls[0].Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments are not JSON: %v (%s)", err, calls[0].Arguments)
	}
	cmd, ok := args["cmd"].([]any)
	if !ok || len(cmd) != 2 || cmd[0] != "ls" {
		t.Errorf("cmd argument = %v", args["cmd"])
	}
	if calls[0].ID == "" {
		t.Error("call id is empty")
	}
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
}

// The model was asked for a bare object and will sometimes fence it anyway.
func TestParseFencedCall(t *testing.T) {
	reply := "```json\n{\"tool\":\"shell\",\"arguments\":{\"cmd\":[\"pwd\"]}}\n```"
	calls, _ := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Name != "shell" {
		t.Errorf("name = %q", calls[0].Name)
	}
}

// Prose around the call is the most common deviation and must not defeat the
// parser, nor be lost as commentary.
func TestParseCallWithSurroundingProse(t *testing.T) {
	reply := "Let me check the directory.\n" +
		`{"tool":"shell","arguments":{"cmd":["ls"]}}` + "\nOne moment."
	calls, text := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	// The commentary is returned so a client that renders text still sees it.
	if text == "" {
		t.Error("surrounding prose was discarded")
	}
}

// A string containing JSON instead of an object is a frequent deviation.
func TestParseArgumentsAsJSONString(t *testing.T) {
	reply := `{"tool":"shell","arguments":"{\"cmd\":[\"whoami\"]}"}`
	calls, _ := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Arguments == "" || calls[0].Arguments[0] != '{' {
		t.Errorf("arguments = %q, want a JSON object", calls[0].Arguments)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
}

// A tool with no parameters is valid and must produce an empty object, not a
// parse failure.
func TestParseCallWithoutArguments(t *testing.T) {
	reply := `{"tool":"shell"}`
	calls, _ := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Arguments != "{}" {
		t.Errorf("arguments = %q, want {}", calls[0].Arguments)
	}
}

// The OpenAI field names turn up when a model is primed by other clients.
func TestParseAlternateFieldNames(t *testing.T) {
	reply := `{"name":"shell","args":{"cmd":["ls"]}}`
	calls, _ := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Name != "shell" {
		t.Errorf("name = %q", calls[0].Name)
	}
}

// An ordinary answer must not be mistaken for a call.
func TestParsePlainProseIsNotACall(t *testing.T) {
	for _, reply := range []string{
		"The file contains three lines.",
		"Here is the config:\n{\"debug\":true,\"port\":8080}",
		"",
	} {
		calls, text := parseToolCalls(reply, testDefs)
		if len(calls) != 0 {
			t.Errorf("reply %q produced %d calls, want 0", reply, len(calls))
		}
		if reply != "" && text != reply {
			t.Errorf("text = %q, want %q", text, reply)
		}
	}
}

// A JSON object that does not name a declared tool is not a call. Without this
// check the model's own example payloads get executed.
func TestParseIgnoresUndeclaredTool(t *testing.T) {
	reply := `{"tool":"nuke_everything","arguments":{}}`
	calls, text := parseToolCalls(reply, testDefs)
	if len(calls) != 0 {
		t.Errorf("got %d calls, want 0 for an undeclared tool", len(calls))
	}
	if text == "" {
		t.Error("the reply was swallowed")
	}
}

// A capitalised name is a spelling mistake, not a different tool.
func TestParseMatchesToolNameCaseInsensitively(t *testing.T) {
	reply := `{"tool":"SHELL","arguments":{"cmd":["ls"]}}`
	calls, _ := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Name != "shell" {
		t.Errorf("name = %q, want the declared spelling", calls[0].Name)
	}
}

// Patches and code contain braces. Brace counting must skip them or the
// extracted object is truncated mid-string.
func TestScanJSONObjectsIgnoresBracesInStrings(t *testing.T) {
	s := `{"tool":"shell","arguments":{"cmd":["echo","{not a brace}"]}} trailing`
	objs := scanJSONObjects(s)
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1", len(objs))
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(objs[0]), &env); err != nil {
		t.Fatalf("extracted object is not valid JSON: %v\n%s", err, objs[0])
	}
}

// Escaped quotes inside a string must not end the string early.
func TestScanJSONObjectsHandlesEscapedQuotes(t *testing.T) {
	s := `{"tool":"shell","arguments":{"cmd":["echo \"hi\""]}}`
	objs := scanJSONObjects(s)
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1", len(objs))
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(objs[0]), &env); err != nil {
		t.Fatalf("extracted object is not valid JSON: %v", err)
	}
}

// The upstream may emit the think block; the stripper runs first, but the
// parser must also cope if a call arrives wrapped in one.
func TestParseCallAfterThinkBlock(t *testing.T) {
	reply := "<think>\x00I should list files\x00</think>\x00" +
		`{"tool":"shell","arguments":{"cmd":["ls"]}}`
	_, answer := splitThink(reply)
	calls, _ := parseToolCalls(answer, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
}

// ---- streaming ------------------------------------------------------------

func TestStreamParserHoldsCallUntilComplete(t *testing.T) {
	p := &toolStreamParser{defs: testDefs}
	var released string
	for _, c := range []string{`{"tool":`, `"shell","argu`, `ments":{"cmd":["ls"]}}`} {
		released += p.observe(c)
	}
	text, calls := p.finish()
	released += text

	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Name != "shell" {
		t.Errorf("name = %q", calls[0].Name)
	}
	if released != "" {
		t.Errorf("call JSON leaked as text: %q", released)
	}
}

// Prose must stream through without being buffered to the end.
func TestStreamParserReleasesProse(t *testing.T) {
	p := &toolStreamParser{defs: testDefs}
	var released string
	for _, c := range []string{"Hello", ", ", "world"} {
		released += p.observe(c)
	}
	text, calls := p.finish()
	released += text

	if len(calls) != 0 {
		t.Errorf("prose produced %d calls", len(calls))
	}
	if released != "Hello, world" {
		t.Errorf("released = %q, want %q", released, "Hello, world")
	}
}

// A leading fence must not be mistaken for prose before the call is visible.
func TestStreamParserHandlesFencePrefix(t *testing.T) {
	p := &toolStreamParser{defs: testDefs}
	var released string
	for _, c := range []string{"```json\n", `{"tool":"shell","arguments":`, `{"cmd":["ls"]}}`} {
		released += p.observe(c)
	}
	text, calls := p.finish()
	released += text
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 (released %q)", len(calls), released)
	}
}

// A buffered reply that turns out not to be a call must be released as text,
// not dropped: the user still needs to see the model's answer.
func TestStreamParserReleasesMalformedCall(t *testing.T) {
	p := &toolStreamParser{defs: testDefs}
	var released string
	for _, c := range []string{`{"tool":"shell"`, `, "arguments": }`} {
		released += p.observe(c)
	}
	text, calls := p.finish()
	released += text
	if len(calls) != 0 {
		t.Errorf("malformed call produced %d calls", len(calls))
	}
	if released == "" {
		t.Error("malformed reply was swallowed instead of released")
	}
}

// ---- native XML syntax ----------------------------------------------------

// The model was trained on this syntax before the relay existed and prefers it
// over the JSON the prompt asks for, so it has to be understood.
func TestParseNativeXMLCall(t *testing.T) {
	reply := `<tool_call><function=shell><parameter=cmd>["ls","-la","/tmp"]</parameter></function></tool_call>`
	calls, text := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 (text %q)", len(calls), text)
	}
	if calls[0].Name != "shell" {
		t.Errorf("name = %q", calls[0].Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments are not JSON: %v (%s)", err, calls[0].Arguments)
	}
	cmd, ok := args["cmd"].([]any)
	if !ok || len(cmd) != 3 || cmd[0] != "ls" {
		t.Errorf("cmd = %v", args["cmd"])
	}
	if text != "" {
		t.Errorf("text = %q, want empty", text)
	}
}

// A scalar parameter is written as bare text, not JSON.
func TestParseNativeXMLCallWithScalarParameter(t *testing.T) {
	reply := `<tool_call><function=read_file><parameter=path>src/main.go</parameter></function></tool_call>`
	calls, _ := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "src/main.go" {
		t.Errorf("path = %v, want the bare string preserved", args["path"])
	}
}

// Several parameters in one call.
func TestParseNativeXMLCallWithMultipleParameters(t *testing.T) {
	reply := `<tool_call><function=shell><parameter=cmd>["git","diff"]</parameter><parameter=cwd>/repo</parameter></function></tool_call>`
	calls, _ := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["cwd"] != "/repo" {
		t.Errorf("cwd = %v", args["cwd"])
	}
	if _, ok := args["cmd"].([]any); !ok {
		t.Errorf("cmd = %v, want an array", args["cmd"])
	}
}

// Prose around the call is kept as commentary.
func TestParseNativeXMLCallWithProse(t *testing.T) {
	reply := "Let me look at the directory.\n" +
		`<tool_call><function=shell><parameter=cmd>["ls"]</parameter></function></tool_call>`
	calls, text := parseToolCalls(reply, testDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if !strings.Contains(text, "look at the directory") {
		t.Errorf("prose was lost: %q", text)
	}
}

// A call to a tool the client never declared must not be executed.
func TestParseNativeXMLIgnoresUndeclaredTool(t *testing.T) {
	reply := `<tool_call><function=rm_rf><parameter=path>/</parameter></function></tool_call>`
	calls, _ := parseToolCalls(reply, testDefs)
	if len(calls) != 0 {
		t.Errorf("got %d calls for an undeclared tool", len(calls))
	}
}

// The streaming parser must hold a native-syntax call back rather than
// releasing it as text.
func TestStreamParserHoldsNativeXMLCall(t *testing.T) {
	p := &toolStreamParser{defs: testDefs}
	var released string
	chunks := []string{"<tool_", "call><function=shell>", "<parameter=cmd>",
		`["ls"]`, "</parameter></function></tool_call>"}
	for _, c := range chunks {
		released += p.observe(c)
	}
	text, calls := p.finish()
	released += text

	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 (released %q)", len(calls), released)
	}
	if released != "" {
		t.Errorf("call markup leaked as text: %q", released)
	}
}

// ---- argument fidelity -----------------------------------------------------

// patchText is the shape Codex sends for apply_patch: a single long argument
// that is not a JSON object at all. It carries newlines, quotes, backslashes
// and angle brackets, every one of which a careless round trip mangles.
const patchText = `*** Begin Patch
*** Update File: internal/httpapi/chat.go
@@ func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
-	body, err := readBody(w, r, s.cfg.Upstream.MaxBodyBytes)
+	// A "quoted" note with a backslash: C:\tmp\payload
+	body, err := readBody(w, r, s.cfg.Upstream.MaxBodyBytes)
+	if err != nil && err.Error() == "a<b && c>d" {
+		return
+	}
 }
*** End Patch`

// patchDefs declares a tool whose argument is long free-form text.
var patchDefs = []ToolDef{
	{Name: "apply_patch", Description: "apply a patch"},
	{Name: "shell", Description: "run a command"},
}

// A patch argument carried as a JSON string must come back byte for byte. The
// JSON escaping is the transport encoding, not part of the value: what the
// tool receives has to be the original text.
func TestPatchArgumentSurvivesJSONEnvelope(t *testing.T) {
	encoded, err := json.Marshal(map[string]any{
		"tool":      "apply_patch",
		"arguments": map[string]any{"patch": patchText},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls, text := parseToolCalls(string(encoded), patchDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 (text %q)", len(calls), text)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments are not JSON: %v (%s)", err, calls[0].Arguments)
	}
	if args["patch"] != patchText {
		t.Errorf("the patch was altered in transit.\n--- got ---\n%s\n--- want ---\n%s",
			args["patch"], patchText)
	}
}

// The same value through the XML syntax the model actually prefers. Here the
// newlines are literal rather than escaped, and the angle brackets must not be
// mistaken for the start of the next tag.
func TestPatchArgumentSurvivesXMLParameter(t *testing.T) {
	reply := "<tool_call><function=apply_patch><parameter=patch>" +
		patchText + "</parameter></function></tool_call>"
	calls, _ := parseToolCalls(reply, patchDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments are not JSON: %v", err)
	}
	if args["patch"] != patchText {
		t.Errorf("the patch was altered in transit.\n--- got ---\n%q\n--- want ---\n%q",
			args["patch"], patchText)
	}
}

// XML entities in a parameter value must be decoded exactly one level.
func TestXMLParameterEntitiesDecodedOnce(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"angle brackets", "if a &lt; b &amp;&amp; c &gt; d {", "if a < b && c > d {"},
		{"quotes", "say &quot;hi&quot; and &apos;bye&apos;", `say "hi" and 'bye'`},
		{"ampersand is not double decoded", "a &amp;amp; b", "a &amp; b"},
		{"a plain ampersand is untouched", "AT&T", "AT&T"},
		{"an entity for an ampersand", "a &#38; b", "a & b"},
		{"no entities at all", "plain text", "plain text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := "<tool_call><function=shell><parameter=cmd>" +
				tc.raw + "</parameter></function></tool_call>"
			calls, _ := parseToolCalls(reply, patchDefs)
			if len(calls) != 1 {
				t.Fatalf("got %d calls, want 1", len(calls))
			}
			var args map[string]string
			if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
				t.Fatalf("arguments are not JSON: %v (%s)", err, calls[0].Arguments)
			}
			if args["cmd"] != tc.want {
				t.Errorf("cmd = %q, want %q", args["cmd"], tc.want)
			}
		})
	}
}

// A value that happens to contain the literal text of a parameter tag as
// source code must not be read as the start of the next parameter. This is the
// failure that silently truncates a patch.
func TestXMLParameterTagInsideValueIsNotATag(t *testing.T) {
	value := `the tag is written "<parameter=x>" in the docs`
	reply := "<tool_call><function=shell><parameter=cmd>" + value +
		"</parameter></function></tool_call>"
	calls, _ := parseToolCalls(reply, patchDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["cmd"] != value {
		t.Errorf("cmd = %q, want %q", args["cmd"], value)
	}
}

// Interior whitespace is data; only the one newline the model uses to lay the
// value out is structural. Trailing spaces in a value must survive, because a
// patch whose lines end in spaces is a different patch.
func TestXMLParameterPreservesInteriorWhitespace(t *testing.T) {
	value := "line one  \n\tindented\nline three"
	reply := "<tool_call><function=apply_patch><parameter=patch>" +
		value + "</parameter></function></tool_call>"
	calls, _ := parseToolCalls(reply, patchDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["patch"] != value {
		t.Errorf("patch = %q, want %q", args["patch"], value)
	}
}

// A patch that is valid JSON on its own (a bare object) must be decoded as
// JSON rather than kept as a string, because the schema asked for an object.
func TestJSONParameterStaysStructured(t *testing.T) {
	reply := `<tool_call><function=shell><parameter=cmd>{"nested":{"deep":[1,2,3]}}</parameter></function></tool_call>`
	calls, _ := parseToolCalls(reply, patchDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	obj, ok := args["cmd"].(map[string]any)
	if !ok {
		t.Fatalf("cmd = %#v, want an object", args["cmd"])
	}
	if _, ok := obj["nested"]; !ok {
		t.Errorf("cmd = %#v", obj)
	}
}

// A shell command with backslashes and quotes must round-trip: the JSON
// envelope's own escaping is transport, not value.
func TestBackslashesSurviveRoundTrip(t *testing.T) {
	value := `grep -E "a\\b" C:\Users\tmp | sed 's/\\/\\\\/g'`
	encoded, err := json.Marshal(map[string]any{
		"tool":      "shell",
		"arguments": map[string]any{"cmd": value},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := parseToolCalls(string(encoded), patchDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["cmd"] != value {
		t.Errorf("cmd = %q, want %q", args["cmd"], value)
	}
}

// An argument carrying an escaped newline in a JSON string must arrive with a
// real newline, not the two characters backslash-n.
func TestEscapedNewlinesDecodeToRealOnes(t *testing.T) {
	encoded := `{"tool":"apply_patch","arguments":{"patch":"line1\nline2\ttabbed"}}`
	calls, _ := parseToolCalls(encoded, patchDefs)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(args["patch"], "line1\nline2") {
		t.Errorf("newlines were not decoded: %q", args["patch"])
	}
	if strings.Contains(args["patch"], `\n`) {
		t.Errorf("a literal backslash-n survived: %q", args["patch"])
	}
}

// The stream parser must reassemble a long XML argument split across many
// chunks without corrupting it.
func TestStreamParserReassemblesLongPatch(t *testing.T) {
	p := &toolStreamParser{defs: patchDefs}
	whole := "<tool_call><function=apply_patch><parameter=patch>" +
		patchText + "</parameter></function></tool_call>"

	// Feed it in small slices to force the buffer to hold partial markup.
	var released string
	const step = 7
	for i := 0; i < len(whole); i += step {
		end := i + step
		if end > len(whole) {
			end = len(whole)
		}
		released += p.observe(whole[i:end])
	}
	text, calls := p.finish()
	released += text

	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 (released %q)", len(calls), released)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["patch"] != patchText {
		t.Errorf("the patches differ.\n--- got ---\n%q\n--- want ---\n%q",
			args["patch"], patchText)
	}
	if released != "" {
		t.Errorf("call markup leaked as text: %q", released)
	}
}
