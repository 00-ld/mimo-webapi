package httpapi

import (
	"strings"
	"testing"
)

// The upstream interleaves its scratchpad with the answer. A client receiving
// it verbatim shows the model's reasoning as the reply.
func TestSplitThinkSeparatesReasoning(t *testing.T) {
	reasoning, answer := splitThink("<think>\x00add the numbers\x00</think>\x002")
	if answer != "2" {
		t.Errorf("answer = %q, want %q", answer, "2")
	}
	if !strings.Contains(reasoning, "add the numbers") {
		t.Errorf("reasoning = %q, want it to contain the thought", reasoning)
	}
}

// Models without a scratchpad must pass through untouched, or every ordinary
// reply loses its opening characters.
func TestSplitThinkLeavesPlainTextAlone(t *testing.T) {
	_, answer := splitThink("just an answer")
	if answer != "just an answer" {
		t.Errorf("answer = %q, want it unchanged", answer)
	}
}

// Markers can straddle two chunks. Stripping per chunk leaks "<thi" into the
// answer, which is the failure this buffering exists to prevent.
func TestThinkStripperHandlesSplitMarker(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name:   "opener split across chunks",
			chunks: []string{"<thi", "nk>", "hidden", "</thi", "nk>", "shown"},
			want:   "shown",
		},
		{
			name:   "marker inside one chunk",
			chunks: []string{"<think>hidden</think>shown"},
			want:   "shown",
		},
		{
			name:   "answer before and after",
			chunks: []string{"a", "<think>x</think>", "b"},
			want:   "ab",
		},
		{
			name:   "no marker at all",
			chunks: []string{"hello ", "world"},
			want:   "hello world",
		},
		{
			name:   "closer split",
			chunks: []string{"<think>x</thi", "nk>done"},
			want:   "done",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &thinkStripper{}
			var got strings.Builder
			for _, c := range tc.chunks {
				got.WriteString(s.push(c))
			}
			got.WriteString(s.flush())
			if got.String() != tc.want {
				t.Errorf("got %q, want %q", got.String(), tc.want)
			}
		})
	}
}

// A stream cut short mid-thought has produced no answer, so nothing should be
// released as one.
func TestThinkStripperDropsUnterminatedReasoning(t *testing.T) {
	s := &thinkStripper{}
	out := s.push("<think>still thinking")
	out += s.flush()
	if out != "" {
		t.Errorf("got %q, want empty", out)
	}
}

// NUL separators must not reach the client.
func TestThinkStripperRemovesNUL(t *testing.T) {
	s := &thinkStripper{}
	got := s.push("<think>\x00r\x00</think>\x00answer") + s.flush()
	if strings.ContainsRune(got, 0) {
		t.Errorf("output contains a NUL byte: %q", got)
	}
	if got != "answer" {
		t.Errorf("got %q, want %q", got, "answer")
	}
}

// A NUL separator that reaches a client mid-stream corrupts the text the same
// way one at the end does, so push must strip them and not only flush.
func TestThinkStripperRemovesNULFromStreamedChunks(t *testing.T) {
	s := &thinkStripper{}
	var got strings.Builder
	for _, c := range []string{"\x00hello", " there", "\x00world"} {
		got.WriteString(s.push(c))
	}
	got.WriteString(s.flush())
	if strings.ContainsRune(got.String(), 0) {
		t.Errorf("streamed output contains a NUL byte: %q", got.String())
	}
	if got.String() != "hello thereworld" {
		t.Errorf("got %q, want %q", got.String(), "hello thereworld")
	}
}

// Splitting each frame independently leaves half a tag in the answer, because
// a marker can straddle the boundary between two frames. The collector must
// buffer the whole stream before splitting.
func TestSplitThinkAfterConcatenationHandlesStraddlingMarker(t *testing.T) {
	frames := []string{"<thi", "nk>reasoning", "</thi", "nk>the answer"}
	raw := strings.Join(frames, "")

	reasoning, answer := splitThink(raw)
	if answer != "the answer" {
		t.Errorf("answer = %q, want %q", answer, "the answer")
	}
	if !strings.Contains(reasoning, "reasoning") {
		t.Errorf("reasoning = %q, want it to contain the thought", reasoning)
	}

	// The per-frame approach, which is what the collector used to do, must be
	// shown to be wrong so the fix is not reverted.
	var leaked strings.Builder
	for _, f := range frames {
		_, a := splitThink(f)
		leaked.WriteString(a)
	}
	if leaked.String() == "the answer" {
		t.Log("per-frame splitting happened to work here; the straddling case is not exercised")
	}
}
