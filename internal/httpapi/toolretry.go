package httpapi

import (
	"strings"
)

// Detecting a malformed tool call.
//
// The relay asks the model for a specific format and the model complies most
// of the time. When it does not, the failure is silent: the reply is returned
// as ordinary text, the client prints it, and the agent stalls with no error
// to act on. Retrying is only worth it for this one case, and only when the
// reply actually looks like a failed attempt — retrying every reply would
// double the cost of every conversation.
//
// A reply is retried when it carries a recognisable fragment of either syntax
// but yielded no call. A reply that never attempted a call is answered as
// text, which is the correct outcome and must not be retried.

// malformedCallMarkers are fragments that appear only when the model tried to
// emit a call. A bare "{" is excluded on purpose: ordinary prose about code
// contains braces constantly, and treating that as a failed call would retry
// a large share of legitimate answers.
var malformedCallMarkers = []string{
	xmlToolOpen,    // <tool_call>
	xmlFuncOpen,    // <function=
	xmlParamOpen,   // <parameter=
	`"tool"`,       // the JSON envelope key
	`"tool_call"`,  //
	`"arguments":`, //
	"<tool_call|",  // some models use a pipe-delimited variant
	"functioncall", // no-separator spelling seen in the wild
	"tool_call>",   // a closer without an opener
}

// looksLikeFailedCall reports whether a reply tried to call a tool but did not
// produce one the parser could read.
//
// It is called only after parsing has already failed, so a true result means
// the attempt was there and the format was wrong.
func looksLikeFailedCall(reply string) bool {
	if strings.TrimSpace(reply) == "" {
		return false
	}
	lower := strings.ToLower(reply)
	for _, m := range malformedCallMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// toolRetryInstruction is appended on the second attempt.
//
// It restates the format and shows both accepted spellings, because a model
// that ignored the first instruction usually ignored it for a reason: it has a
// competing habit. Naming the habit explicitly is what makes the second
// attempt land.
const toolRetryInstruction = `

# Correction

Your previous reply attempted a tool call but did not follow the required format, so it could not be executed.

Reply with the tool call in this exact format and nothing else:

<tool_call>
<function=TOOL_NAME>
<parameter=PARAMETER_NAME>VALUE</parameter>
</function>
</tool_call>

Or equivalently, as a single JSON object on one line:

{"tool": "TOOL_NAME", "arguments": {"PARAMETER_NAME": VALUE}}

Rules:
- Use only a tool name listed above.
- Parameters must be the exact names from the schema.
- Emit no prose, no explanation, and no code fence around the call.
`
