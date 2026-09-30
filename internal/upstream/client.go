// Package upstream speaks the MiMo Studio *web* backend protocol.
//
// It is not the OpenAI-compatible developer API. The web backend expects a
// cookie-authenticated POST of a conversation-shaped payload and answers with
// a named-event SSE stream, so this package owns both the request shape and
// the frame-to-chunk decoding.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"mimowebapi/internal/config"
	"mimowebapi/internal/session"
	"mimowebapi/internal/util"
)

// Endpoint paths on the web backend.
const (
	PathChat    = "/open-apis/bot/chat"
	PathConfig  = "/open-apis/bot/config"
	PathUser    = "/open-apis/user/mi/get"
	PathLogin   = "/open-apis/user/mi/get"
	eventPrefix = "event:"
	dataPrefix  = "data:"
)

// Media is an uploaded attachment reference.
type Media struct {
	URL  string `json:"url,omitempty"`
	Name string `json:"name,omitempty"`
	Size int64  `json:"size,omitempty"`
	Type string `json:"type,omitempty"`
}

// ModelConfig is the per-request model block the web client sends.
type ModelConfig struct {
	EnableThinking  bool    `json:"enableThinking"`
	WebSearchStatus string  `json:"webSearchStatus"`
	Model           string  `json:"model"`
	Temperature     float64 `json:"temperature,omitempty"`
	TopP            float64 `json:"topP,omitempty"`
}

// ChatRequest is the payload the web client POSTs to PathChat.
//
// The web backend is stateless with respect to conversation history on the
// server side for a first turn, but it does maintain server-side conversation
// state: passing every prior turn as `query` is what keeps multi-turn context
// intact without server bookkeeping.
type ChatRequest struct {
	MsgID            string      `json:"msgId"`
	ConversationID   string      `json:"conversationId"`
	Query            string      `json:"query"`
	IsEditedQuery    bool        `json:"isEditedQuery"`
	SceneType        *string     `json:"sceneType"`
	Params           any         `json:"params"`
	ModelConfig      ModelConfig `json:"modelConfig"`
	MultiMedias      []Media     `json:"multiMedias"`
	PreviousDialogID *string     `json:"previousDialogueId,omitempty"`
}

// Frame is one decoded SSE event from the web backend.
type Frame struct {
	Event   string
	Content string
	Raw     map[string]any
	Usage   *Usage
}

// Usage is the token accounting the backend reports on the `usage` event.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Client is a cookie-authenticated web-backend client.
type Client struct {
	cfg  *config.Config
	pool *session.Pool
	http *http.Client
	base *url.URL
}

// New builds a client. The HTTP transport disables compression so SSE frames
// reach the caller as the backend emits them; a compressed body would have to
// be decoded first, which destroys streaming latency.
func New(cfg *config.Config, pool *session.Pool) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.Upstream.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse upstream base url: %w", err)
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: time.Duration(cfg.Upstream.ConnectTimeout) * time.Second,
		ResponseHeaderTimeout: time.Duration(
			cfg.Upstream.ResponseTimeout) * time.Second,
		DisableCompression: true,
	}
	return &Client{
		cfg:  cfg,
		pool: pool,
		base: base,
		http: &http.Client{
			Transport: transport,
			// No client-level timeout: a streaming completion legitimately
			// runs longer than any fixed deadline. Cancellation is driven by
			// the caller's context instead.
			Timeout: 0,
		},
	}, nil
}

// SessionStatuses exposes pool health for the /status endpoint.
func (c *Client) SessionStatuses() []session.Status { return c.pool.Statuses() }

// Pool exposes the session pool so newly authorised accounts can be added
// without rebuilding the client.
func (c *Client) Pool() *session.Pool { return c.pool }

// CookieHeader builds the Cookie header for a session.
func (c *Client) cookieHeader(s config.Session) string { return s.Header() }

// headers returns the header set the browser client would send.
func (c *Client) headers(s config.Session) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream, application/json")
	h.Set("Accept-Language", orDefault(c.cfg.Upstream.Locale, "zh-CN"))
	h.Set("x-timeZone", orDefault(c.cfg.Upstream.TimeZone, "Asia/Shanghai"))
	h.Set("User-Agent", orDefault(c.cfg.Upstream.UserAgent, config.DefaultUserAgent))
	h.Set("Origin", c.base.Scheme+"://"+c.base.Host)
	h.Set("Referer", c.base.String()+"/")
	h.Set("Cookie", c.cookieHeader(s))
	return h
}

// requestURL builds the chat URL.
//
// The `xiaomichatbot_ph` value must appear as a QUERY parameter, not only as a
// cookie. The real web client sends it both ways, and the backend's auth gate
// rejects the request if the query parameter is absent — with the same 401 it
// returns for a missing cookie, which is what makes this so easy to misread.
// The value is read from the session's own cookie so the two can never drift.
func (c *Client) requestURL(path string, s config.Session) string {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	q := u.Query()
	if ph := phFromSession(s); ph != "" {
		q.Set("xiaomichatbot_ph", ph)
	}
	if c.cfg.Upstream.ClientID != "" {
		q.Set("clientId", c.cfg.Upstream.ClientID)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// phFromSession finds the anti-fraud `ph` cookie for a session.
//
// The name carries an environment prefix (`xiaomichatbot_ph` in production),
// so it is matched by suffix rather than exactly.
func phFromSession(s config.Session) string {
	for _, ck := range s.Cookies {
		if ck.Name == "ph" || strings.HasSuffix(ck.Name, "_ph") {
			return ck.Value
		}
	}
	return ""
}

// BackendError is a structured failure from the web backend.
type BackendError struct {
	Status    int
	Code      int
	Body      string
	LoginURL  string
	Retryable bool
}

func (e *BackendError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("upstream http %d code %d: %s", e.Status, e.Code, truncate(e.Body, 300))
	}
	return fmt.Sprintf("upstream http %d: %s", e.Status, truncate(e.Body, 300))
}

// IsAuthError reports whether the backend rejected our cookies. The web
// backend answers unauthenticated requests with HTTP 200/401 plus a
// `loginUrl` field, so status alone is not enough to detect it.
func IsAuthError(err error) bool {
	var be *BackendError
	if errors.As(err, &be) {
		return be.Code == 401 || be.Status == 401 || be.LoginURL != ""
	}
	return false
}

// IsBannedError reports an account-level block. The web backend signals this
// with 461 on the chat endpoint.
func IsBannedError(err error) bool {
	var be *BackendError
	if errors.As(err, &be) {
		return be.Status == 451 || be.Status == 461
	}
	return false
}

// QueryTooLongError reports that the composed query exceeded the web
// backend's per-request text ceiling.
//
// This is neither a transport failure nor an upstream outage: the backend
// answers in well under a second with a normal 200 and a well-formed `error`
// frame. It is a deterministic property of the request, so it must never be
// classified as retryable — a client that retries it simply resends the
// identical oversized payload forever instead of compacting its history.
//
// It is deliberately a distinct type rather than a *BackendError: the
// condition is discovered inside the SSE stream, so it has no HTTP status of
// its own to carry.
type QueryTooLongError struct {
	// Message is the backend's own wording, preserved for the operator.
	Message string
	// QueryChars is the composed query length that was rejected.
	QueryChars int
	// Limit is the ceiling the relay enforces, when known.
	Limit int
}

func (e *QueryTooLongError) Error() string {
	if e.Limit > 0 {
		return fmt.Sprintf("query too long: %d characters sent, upstream accepts about %d",
			e.QueryChars, e.Limit)
	}
	return fmt.Sprintf("query too long: %d characters sent; %s", e.QueryChars, e.Message)
}

// IsQueryTooLong reports whether the upstream rejected the request purely
// because the composed query was too large.
func IsQueryTooLong(err error) bool {
	var q *QueryTooLongError
	return errors.As(err, &q)
}

// tooLongMarkers are the backend's own phrasings for an over-length query.
//
// The backend answers with HTTP 200 and an ordinary SSE stream whose `error`
// frame carries this prose, so the message is the only available signal.
// Matching uses stable fragments rather than the whole sentence: the trailing
// advice is marketing copy that can be reworded at any time.
var tooLongMarkers = []string{
	"文本超长",
	"内容超长",
	"超长啦",
	"text too long",
	"too long",
	"context length exceeded",
	"maximum context length",
	"reduce the length",
}

// LooksQueryTooLong reports whether an upstream error message is the
// over-length rejection. It is exported so the HTTP layer can reclassify a
// failure that reaches it as a bare frame string.
func LooksQueryTooLong(msg string) bool {
	low := strings.ToLower(msg)
	for _, m := range tooLongMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// NewQueryTooLong builds the typed error for a rejected over-length query.
func NewQueryTooLong(message string, queryChars, limit int) error {
	return &QueryTooLongError{Message: message, QueryChars: queryChars, Limit: limit}
}

// ChatStream is an open streaming completion.
type ChatStream struct {
	Frames  <-chan Frame
	release func(error)
	body    io.ReadCloser
	// err carries a transport-level failure that ended the stream early, so a
	// caller can tell a truncated response from a complete one.
	err chan error
	// Model echoes the model code the backend accepted.
	Model string
}

// ReadError reports a transport failure that cut the stream short.
//
// It must be consulted after Frames closes. A nil result means the backend
// ended the stream on its own and the accumulated frames are the whole reply;
// a non-nil result means the reply is incomplete and must not be presented as
// finished. It is safe to call more than once and after Close.
func (s *ChatStream) ReadError() error {
	if s.err == nil {
		return nil
	}
	select {
	case err := <-s.err:
		return err
	default:
		return nil
	}
}

// Close releases the stream and reports success or failure to the pool.
func (s *ChatStream) Close(err error) {
	if s.body != nil {
		_ = s.body.Close()
	}
	if s.release != nil {
		s.release(err)
		s.release = nil
	}
}

// Chat opens a streaming completion and returns decoded frames.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatStream, error) {
	if c.pool.Size() == 0 {
		return nil, session.ErrNoSession
	}

	// The web backend rejects an over-length query with a normal 200 and an
	// in-band `error` frame, which costs a round-trip and a lease to discover.
	// The ceiling is a stable property of the account, so it is enforced here
	// first and the caller gets an accurate, permanent error immediately.
	//
	// The ceiling counts characters, not bytes: a CJK-heavy prompt is 3 bytes
	// per character in UTF-8, so a byte comparison would reject a request that
	// the upstream happily accepts (and vice versa for ASCII). Counting runes
	// is what makes the local guard agree with the backend.
	if limit := c.cfg.Upstream.MaxQueryChars; limit > 0 {
		if n := utf8.RuneCountInString(req.Query); n > limit {
			return nil, NewQueryTooLong("", n, limit)
		}
	}

	var lastErr error
	// Try a few sessions before giving up: one dead cookie should not fail
	// the request when a healthy one is configured next to it.
	attempts := c.pool.Size()
	if attempts > 3 {
		attempts = 3
	}
	for i := 0; i < attempts; i++ {
		sess, release, err := c.pool.Lease()
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		stream, err := c.chatOnce(ctx, sess, req)
		if err != nil {
			release(classify(err))
			lastErr = err
			if !retryable(err) {
				return nil, err
			}
			continue
		}
		stream.release = release
		return stream, nil
	}
	return nil, lastErr
}

// Probe checks that a freshly captured session is actually accepted by the
// backend, without going through the pool.
//
// The authorisation wizard uses this so it can tell the user "you are logged
// in and it works" instead of discovering a dead token on the first real
// request. It sends the smallest possible completion and only reads the first
// frames, so the cost is one short request.
func (c *Client) Probe(ctx context.Context, sess config.Session) error {
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	stream, err := c.chatOnce(probeCtx, sess, ChatRequest{
		MsgID:          "auth-probe",
		ConversationID: "auth-probe",
		Query:          "hi",
		ModelConfig:    ModelConfig{Model: c.cfg.Models.Default},
	})
	if err != nil {
		return err
	}
	defer stream.Close(nil)

	// Any well-formed frame proves the session was accepted; the first token
	// or an explicit error both settle the question, so there is no need to
	// drain the stream.
	for frame := range stream.Frames {
		switch frame.Event {
		case "error":
			return fmt.Errorf("backend rejected the probe: %s", frame.Content)
		default:
			return nil
		}
	}
	return nil
}

func (c *Client) chatOnce(ctx context.Context, sess config.Session,
	req ChatRequest) (*ChatStream, error) {

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode upstream request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.requestURL(PathChat, sess), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	httpReq.Header = c.headers(sess)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, decodeBackendError(resp.StatusCode, raw)
	}

	// A JSON content-type instead of text/event-stream means the backend
	// answered with a control payload (login prompt, ban notice, quota) rather
	// than opening a stream.
	if ct := resp.Header.Get("Content-Type"); ct != "" &&
		!strings.Contains(ct, "event-stream") {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, decodeBackendError(resp.StatusCode, raw)
	}

	frames := make(chan Frame, 32)
	streamErr := make(chan error, 1)
	go func() {
		defer close(frames)
		parseSSE(ctx, resp.Body, frames, streamErr)
	}()

	return &ChatStream{
		Frames: frames,
		err:    streamErr,
		body:   resp.Body,
		Model:  req.ModelConfig.Model,
	}, nil
}

// parseSSE decodes named-event SSE frames into the channel.
//
// The web backend emits frames as `event: <name>` followed by `data: <json>`
// with a blank line terminator. `data:` lines are accumulated in case the
// backend ever splits a JSON payload across multiple lines, which the SSE
// spec permits.
func parseSSE(ctx context.Context, r io.Reader, out chan<- Frame, errCh chan<- error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	var event string
	var data strings.Builder

	flush := func() {
		if event == "" && data.Len() == 0 {
			return
		}
		f := Frame{Event: event, Content: ""}
		raw := strings.TrimSpace(data.String())
		if raw != "" && raw != "[DONE]" {
			var obj map[string]any
			if err := json.Unmarshal([]byte(raw), &obj); err == nil {
				f.Raw = obj
				if s, ok := obj["content"].(string); ok {
					f.Content = s
				} else if s, ok := obj["text"].(string); ok {
					f.Content = s
				}
			} else {
				// Not JSON: treat the payload as literal content. The backend
				// has been observed sending bare strings on some events.
				f.Content = raw
			}
		}
		if f.Event == "usage" && f.Raw != nil {
			f.Usage = parseUsage(f.Raw)
		}
		select {
		case out <- f:
		case <-ctx.Done():
		}
		event = ""
		data.Reset()
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if v, ok := trimPrefixFold(line, eventPrefix); ok {
			event = strings.TrimSpace(v)
			continue
		}
		if v, ok := trimPrefixFold(line, dataPrefix); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(v))
			continue
		}
		// `id:`, `retry:` and comments (`:`) are ignored.
	}
	// A read failure is not an end of stream.
	//
	// Without this check a connection reset, a truncated chunked body or an
	// idle-timeout kill looks exactly like the backend finishing normally, so
	// the caller renders a half-generated answer as a complete one. Reporting
	// the error is what lets the caller distinguish "the model finished" from
	// "the transport died", which is the difference between a correct reply
	// and silent truncation.
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			select {
			case errCh <- fmt.Errorf("upstream stream read failed: %w", err):
			default:
			}
		}
		return
	}
	flush()
}

// parseUsage normalizes the backend's usage object.
//
// The live format is camelCase and carries a nested `nativeUsage` block with
// the provider's own numbers:
//
//	{"promptTokens":2370,"completionTokens":149,"totalTokens":2519,
//	 "nativeUsage":{"completion_tokens":149,"prompt_tokens":2370,
//	                "total_tokens":2519, ...}}
//
// Both spellings are accepted because earlier model revisions used the
// snake_case form at the top level.
func parseUsage(raw map[string]any) *Usage {
	u := &Usage{}
	u.PromptTokens = intField(raw, "promptTokens", "prompt_tokens",
		"inputTokens", "input_tokens")
	u.CompletionTokens = intField(raw, "completionTokens", "completion_tokens",
		"outputTokens", "output_tokens")
	u.TotalTokens = intField(raw, "totalTokens", "total_tokens")

	// Prefer the provider's own accounting when present: it distinguishes
	// cached and reasoning tokens, which the outer numbers fold together.
	if nested, ok := raw["nativeUsage"].(map[string]any); ok {
		if v := intField(nested, "prompt_tokens", "promptTokens"); v > 0 {
			u.PromptTokens = v
		}
		if v := intField(nested, "completion_tokens", "completionTokens"); v > 0 {
			u.CompletionTokens = v
		}
		if v := intField(nested, "total_tokens", "totalTokens"); v > 0 {
			u.TotalTokens = v
		}
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u
}

func intField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n)
			}
		}
	}
	return 0
}

func decodeBackendError(status int, raw []byte) error {
	be := &BackendError{Status: status, Body: string(raw)}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		be.Code = intField(obj, "code")
		if s, ok := obj["loginUrl"].(string); ok {
			be.LoginURL = s
		}
		if s, ok := obj["message"].(string); ok && s != "" {
			be.Body = s
		} else if s, ok := obj["msg"].(string); ok && s != "" {
			be.Body = s
		}
	}
	be.Retryable = status >= 500 || status == http.StatusTooManyRequests
	if be.Code == 0 && be.LoginURL == "" {
		be.Retryable = status >= 500 || status == http.StatusTooManyRequests
	}
	return be
}

// classify wraps an error so the pool knows whether to park the session.
func classify(err error) error {
	if IsAuthError(err) || IsBannedError(err) {
		return &session.FatalError{Reason: reasonFor(err), Err: err}
	}
	return err
}

func reasonFor(err error) string {
	switch {
	case IsBannedError(err):
		return "account_banned"
	case IsAuthError(err):
		return "cookies_expired"
	default:
		return "upstream_error"
	}
}

func retryable(err error) bool {
	if IsAuthError(err) || IsBannedError(err) {
		return true // a *different* session may still work
	}
	// An over-length query is a property of the request, not of the session.
	// Retrying it against another cookie reproduces the identical rejection
	// while burning a lease, so it is never retryable.
	if IsQueryTooLong(err) {
		return false
	}
	var be *BackendError
	if errors.As(err, &be) {
		return be.Retryable
	}
	return true // transport-level failures are worth another session
}

func trimPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// NewChatRequest assembles a request for a single query.
//
// conversationID pins the server-side conversation. The web backend keeps
// per-conversation state, so a stable id across turns is what preserves
// context — passing the full history in `query` alone is not reliable.
func NewChatRequest(conversationID, model string, cfg ModelConfig, query string,
	medias []Media) ChatRequest {
	scene := (*string)(nil)
	if conversationID == "" {
		conversationID = util.NewID()
	}
	return ChatRequest{
		MsgID:          util.NewID(),
		ConversationID: conversationID,
		Query:          query,
		IsEditedQuery:  false,
		SceneType:      scene,
		Params:         map[string]any{},
		ModelConfig:    cfg,
		MultiMedias:    append([]Media{}, medias...),
	}
}
