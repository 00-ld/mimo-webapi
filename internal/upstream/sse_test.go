package upstream

import (
	"context"
	"strings"
	"testing"
)

func TestParseSSENamedEvents(t *testing.T) {
	// Shape mirrored from the web client's own SSE reader: an `event:` line
	// followed by a `data:` line, blank-line terminated.
	raw := "event: dialogId\ndata: {\"content\":\"dlg-1\"}\n\n" +
		"event: message\ndata: {\"content\":\"Hello\"}\n\n" +
		"event: message\ndata: {\"content\":\" world\"}\n\n" +
		"event: usage\ndata: {\"prompt_tokens\":11,\"completion_tokens\":2}\n\n" +
		"event: finish\ndata: {}\n\n"

	ch := make(chan Frame, 16)
	parseSSE(context.Background(), strings.NewReader(raw), ch)
	close(ch)

	var got []Frame
	for f := range ch {
		got = append(got, f)
	}
	if len(got) != 5 {
		t.Fatalf("want 5 frames, got %d: %+v", len(got), got)
	}
	if got[0].Event != "dialogId" || got[0].Content != "dlg-1" {
		t.Errorf("frame 0 = %+v", got[0])
	}
	if got[1].Content != "Hello" || got[2].Content != " world" {
		t.Errorf("text frames = %q %q", got[1].Content, got[2].Content)
	}
	if got[3].Usage == nil {
		t.Fatalf("usage frame lost its usage object: %+v", got[3])
	}
	if got[3].Usage.PromptTokens != 11 || got[3].Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", got[3].Usage)
	}
	if got[3].Usage.TotalTokens != 13 {
		t.Errorf("total should be derived, got %d", got[3].Usage.TotalTokens)
	}
	if got[4].Event != "finish" {
		t.Errorf("last frame = %+v", got[4])
	}
}

// Multi-line data is legal SSE and must be joined, not truncated to line one.
func TestParseSSEMultilineData(t *testing.T) {
	raw := "event: message\ndata: {\"content\":\ndata: \"split\"}\n\n"
	ch := make(chan Frame, 4)
	parseSSE(context.Background(), strings.NewReader(raw), ch)
	close(ch)

	var frames []Frame
	for f := range ch {
		frames = append(frames, f)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(frames))
	}
	if frames[0].Content != "split" {
		t.Errorf("content = %q", frames[0].Content)
	}
}

// A bare-string payload has been observed on some events; it must survive as
// literal content rather than being dropped.
func TestParseSSENonJSONPayload(t *testing.T) {
	raw := "event: error\ndata: upstream exploded\n\n"
	ch := make(chan Frame, 4)
	parseSSE(context.Background(), strings.NewReader(raw), ch)
	close(ch)
	f := <-ch
	if f.Event != "error" || f.Content != "upstream exploded" {
		t.Errorf("frame = %+v", f)
	}
}

// Comments and unknown fields must be ignored without corrupting the frame.
func TestParseSSEIgnoresCommentsAndID(t *testing.T) {
	raw := ": keep-alive\nid: 42\nretry: 1000\n" +
		"event: message\ndata: {\"content\":\"ok\"}\n\n"
	ch := make(chan Frame, 4)
	parseSSE(context.Background(), strings.NewReader(raw), ch)
	close(ch)

	var frames []Frame
	for f := range ch {
		frames = append(frames, f)
	}
	if len(frames) != 1 || frames[0].Content != "ok" {
		t.Fatalf("frames = %+v", frames)
	}
}

// A trailing frame without a final blank line must still be delivered: some
// proxies close the body without a terminating newline.
func TestParseSSEFlushesTrailingFrame(t *testing.T) {
	raw := "event: message\ndata: {\"content\":\"tail\"}"
	ch := make(chan Frame, 4)
	parseSSE(context.Background(), strings.NewReader(raw), ch)
	close(ch)
	f := <-ch
	if f.Content != "tail" {
		t.Errorf("content = %q", f.Content)
	}
}

// Cancellation must not block a producer that is waiting to hand off a frame.
func TestParseSSEStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan Frame, 0) // unbuffered: any send would block forever
	done := make(chan struct{})
	go func() {
		parseSSE(ctx, strings.NewReader("event: message\ndata: {}\n\n"), ch)
		close(done)
	}()
	<-done
}
