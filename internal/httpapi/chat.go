package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"mimowebapi/internal/apikeys"
	"mimowebapi/internal/config"
	"mimowebapi/internal/session"
	"mimowebapi/internal/upstream"
)

// version mirrors main.version for the root and health payloads.
const version = "2.0.0"

// Frames emitted by the MiMo web backend, discovered from its own SSE reader.
const (
	frameMessage        = "message"
	frameDialogID       = "dialogId"
	frameFinish         = "finish"
	frameUsage          = "usage"
	frameError          = "error"
	frameWebSearch      = "web_search"
	frameSensitiveQuery = "sensitive_query"
	frameSensitiveTitle = "sensitive_title"
	frameDoc            = "doc"
	frameTipRatio       = "tip_ratio"
	frameTipTruncate    = "tip_truncate"
)

// Server holds everything the HTTP handlers need.
type Server struct {
	cfg           *config.Config
	client        *upstream.Client
	conversations *upstream.ConversationMap
	log           *slog.Logger
	tokens        []string
	keys          *apikeys.Store
	startedAt     time.Time

	account    *authManager
	sessionsMu sync.Mutex

	// agentsEnvOverride pins the filesystem view the onboarding endpoints
	// operate on. Only tests set it, so no production path can reach a
	// location the operator did not mean.
	agentsEnvOverride *agentEnv
}

// NewServer wires the handlers.
func NewServer(cfg *config.Config, client *upstream.Client, log *slog.Logger,
	keys *apikeys.Store) *Server {
	return &Server{
		cfg:           cfg,
		client:        client,
		conversations: upstream.NewConversationMap(1024),
		log:           log,
		tokens:        cfg.ClientTokens,
		keys:          keys,
		startedAt:     time.Now(),
		account:       newAuthManager(),
	}
}

// Routes builds the mux.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/admin/api/", s.handleAdmin)
	// The console is only useful with a password to log in with; without one
	// it would render a login form that can never succeed.
	if s.cfg.Admin.Console && s.cfg.Admin.Password != "" {
		mux.HandleFunc("/console", s.handleConsole)
		mux.HandleFunc("/console/", s.handleConsole)
	}
	mux.HandleFunc("/", s.handleRoot)

	mux.Handle("/status", s.auth(http.HandlerFunc(s.handleStatus)))
	// The canonical route plus the spellings aggregators probe. All three land
	// on the same handler; only the presence of the endpoint matters to a
	// caller that is deciding whether this upstream is usable.
	mux.Handle("/v1/models", s.auth(http.HandlerFunc(s.handleModels)))
	mux.Handle("/v1/models/", s.auth(http.HandlerFunc(s.handleModels)))
	mux.Handle("/models", s.auth(http.HandlerFunc(s.handleModels)))
	mux.Handle("/models/", s.auth(http.HandlerFunc(s.handleModels)))
	mux.Handle("/v1/chat/completions", s.auth(http.HandlerFunc(s.handleChatCompletions)))
	mux.Handle("/v1/completions", s.auth(http.HandlerFunc(s.handleChatCompletions)))
	mux.Handle("/v1/messages", s.auth(http.HandlerFunc(s.handleAnthropicMessages)))
	// The Responses API, under every spelling clients use. An agent that
	// probes one of these, gets a 404, and concludes the upstream is dead is
	// the reason all of them are registered.
	mux.Handle("/v1/responses", s.auth(http.HandlerFunc(s.handleResponses)))
	mux.Handle("/responses", s.auth(http.HandlerFunc(s.handleResponses)))
	return mux
}

// identity is who a request belongs to.
type identity struct {
	key *apikeys.Key // nil for a static client_token
}

// auth wraps a handler with API key checking.
//
// Two credential kinds are accepted: an issued key from the store (which
// carries quota and per-user accounting) and a static client_token from the
// config (which does not). Both are compared in constant time.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.authenticate(r)
		if err != nil {
			status, code, msg := keyAuthError(err)
			writeError(w, status, "invalid_request_error", code, msg)
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}

// authenticate resolves the presented credential.
func (s *Server) authenticate(r *http.Request) (*identity, error) {
	presented := extractToken(r)
	if presented == "" {
		return nil, apikeys.ErrNotFound
	}
	// Static config tokens first: they are the operator's own and bypass quota.
	match := false
	for _, t := range s.tokens {
		if constTimeEqual(t, presented) {
			match = true
		}
	}
	if match {
		return &identity{}, nil
	}
	if s.keys != nil && s.keys.Count() > 0 {
		k, err := s.keys.Authenticate(presented)
		if err != nil {
			return nil, err
		}
		return &identity{key: k}, nil
	}
	return nil, apikeys.ErrNotFound
}

// authorized reports whether the request carries any valid credential. It is
// used by the lightweight endpoints that do not need per-key accounting.
func (s *Server) authorized(r *http.Request) bool {
	_, err := s.authenticate(r)
	return err == nil
}

// handleRoot gives a human something useful instead of a bare 404.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "mimowebapi",
		"version": version,
		"endpoints": []string{
			"POST /v1/chat/completions", "POST /v1/messages",
			"POST /v1/responses",
			"GET  /v1/models", "GET  /healthz",
		},
		"console": s.cfg.Admin.Console,
	})
}

func extractToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"sessions": s.client.SessionStatuses(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"upstream": s.cfg.Upstream.BaseURL,
		"models":   s.cfg.Models.List,
		"default":  s.cfg.Models.Default,
		"sessions": s.client.SessionStatuses(),
	})
}

// handleModels answers the OpenAI model list locally. The web backend does
// expose a catalogue, but answering from config keeps startup independent of
// a live upstream call and avoids a cookie round-trip per discovery.
// modelEntry is one row of a model listing.
//
// The shape follows OpenAI's, plus the extra fields aggregators read when they
// populate a model picker. Unknown extra keys are ignored by strict clients and
// used by the ones that want them, so sending them is safe both ways.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// DisplayName is what sub2api and friends show in the picker. Without it
	// the raw id is shown, which is fine but less readable.
	DisplayName string `json:"display_name,omitempty"`
	// ContextLength and MaxOutput drive the "context" column in aggregators.
	ContextLength int `json:"context_length,omitempty"`
	MaxOutput     int `json:"max_output,omitempty"`
	// Capabilities is read by clients that filter on feature support.
	Capabilities []string `json:"capabilities,omitempty"`
	// Permission mirrors the legacy OpenAI shape. Some older clients read it
	// and mis-handle a listing where it is absent.
	Permission []modelPermission `json:"permission,omitempty"`
}

type modelPermission struct {
	ID                 string `json:"id"`
	Object             string `json:"object"`
	Created            int64  `json:"created"`
	AllowCreateEngine  bool   `json:"allow_create_engine"`
	AllowSampling      bool   `json:"allow_sampling"`
	AllowLogprobs      bool   `json:"allow_logprobs"`
	AllowSearchIndices bool   `json:"allow_search_indices"`
	AllowView          bool   `json:"allow_view"`
	AllowFineTuning    bool   `json:"allow_fine_tuning"`
	Organization       string `json:"organization"`
	Group              string `json:"group"`
	IsBlocking         bool   `json:"is_blocking"`
}

// stableModelTime is a fixed timestamp for every model.
//
// A model's creation time is not meaningful here and must not change between
// calls: a listing whose entries appear to be created on every request makes
// caches and diffing clients re-sync endlessly.
const stableModelTime int64 = 1735689600 // 2025-01-01T00:00:00Z

func (s *Server) modelEntries() []modelEntry {
	out := make([]modelEntry, 0, len(s.cfg.Models.List))
	for _, id := range s.cfg.Models.List {
		e := modelEntry{
			ID:           id,
			Object:       "model",
			Created:      stableModelTime,
			OwnedBy:      "xiaomi-mimo",
			DisplayName:  id,
			Capabilities: []string{"chat", "streaming"},
		}
		if m, ok := s.cfg.Models.Meta[id]; ok {
			if m.DisplayName != "" {
				e.DisplayName = m.DisplayName
			}
			e.ContextLength = m.ContextLength
			e.MaxOutput = m.MaxOutput
			if len(m.Capabilities) > 0 {
				e.Capabilities = m.Capabilities
			}
		}
		// The picker renders a row even when context is unknown; supplying a
		// plausible default beats showing nothing.
		if e.ContextLength == 0 {
			e.ContextLength = 65536
		}
		if e.MaxOutput == 0 {
			e.MaxOutput = 8192
		}
		e.Permission = []modelPermission{{
			ID: "modelperm-" + id, Object: "model_permission",
			Created: stableModelTime, AllowSampling: true, AllowView: true,
		}}
		out = append(out, e)
	}
	return out
}

// handleModels serves the model listing and single-model lookups.
//
// Aggregators probe several spellings before they settle, so the same handler
// answers /v1/models, /models and /v1/models/{id}. A 404 on any of them reads
// as "this upstream has no models" and the account is rejected.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error",
			"method_not_allowed", "use GET")
		return
	}

	entries := s.modelEntries()

	// A trailing path segment means a single-model lookup.
	rest := strings.TrimPrefix(r.URL.Path, "/")
	rest = strings.TrimPrefix(rest, "v1/")
	rest = strings.TrimPrefix(rest, "models")
	rest = strings.Trim(rest, "/")

	if rest != "" {
		for _, e := range entries {
			if e.ID == rest {
				writeJSON(w, http.StatusOK, e)
				return
			}
		}
		writeError(w, http.StatusNotFound, "invalid_request_error",
			"model_not_found", "model "+rest+" is not available")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": entries})
}

// handleChatCompletions serves POST /v1/chat/completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
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
	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"missing_messages", "messages must not be empty")
		return
	}

	history, finalQuery, err := toTurns(req.Messages, s.cfg.Behavior.SystemPromptMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_messages", err.Error())
		return
	}

	// The upstream takes one plain-text query and knows nothing about tools, so
	// the definitions are rendered into the prompt and the reply is parsed
	// back into tool calls afterwards.
	defs := parseToolDefs(req.Tools)

	query := upstream.ComposeQuery(history, finalQuery, s.cfg.Behavior.SystemPromptMode)
	if block := renderPromptBlock(defs); block != "" {
		query += block
	}

	model := s.cfg.ResolveModel(req.Model)
	mc := upstream.ModelConfig{
		EnableThinking:  s.thinkingEnabled(req),
		WebSearchStatus: s.webSearchStatus(req),
		Model:           model,
	}
	if req.Temperature != nil {
		mc.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		mc.TopP = *req.TopP
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

	id := newCompletionID("chatcmpl")
	if req.Stream {
		s.streamOpenAI(w, ctx, stream, id, model, defs)
		return
	}

	// A non-streaming reply can be checked before anything is sent, so a
	// malformed tool call is worth one corrected attempt.
	s.collectOpenAIWithRetry(w, ctx, stream, id, model, req, defs, upReq)
}

func (s *Server) thinkingEnabled(req ChatCompletionRequest) bool {
	if req.Thinking != nil {
		return *req.Thinking
	}
	switch strings.ToLower(req.ReasoningEffort) {
	case "none", "off", "disabled":
		return false
	case "":
		return s.cfg.Behavior.EnableThinkingDefault
	default:
		return true
	}
}

func (s *Server) webSearchStatus(req ChatCompletionRequest) string {
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

// streamOpenAI forwards upstream frames as OpenAI streaming chunks.
func (s *Server) streamOpenAI(w http.ResponseWriter, ctx context.Context,
	stream *upstream.ChatStream, id, model string, defs []ToolDef) {

	sw, err := newSSEWriter(w)
	if err != nil {
		stream.Close(err)
		return
	}

	created := time.Now().Unix()
	first := &Choice{Index: 0, Delta: &RespMsg{Role: "assistant"}, FinishReason: nil}

	// A client that disconnects mid-stream must cancel the upstream read,
	// otherwise the goroutine and the account's server-side generation both
	// keep running.
	go func() {
		<-ctx.Done()
		stream.Close(ctx.Err())
	}()

	if err := sw.event("", ChatCompletion{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []Choice{*first},
	}); err != nil {
		stream.Close(err)
		return
	}

	var usage *Usage
	finishReason := "stop"
	var streamErr error
	strip := &thinkStripper{}
	toolParser := &toolStreamParser{defs: defs}

	for frame := range stream.Frames {
		switch frame.Event {
		case frameMessage:
			if frame.Content == "" {
				continue
			}
			// The upstream interleaves its scratchpad with the answer. This
			// protocol has a field for reasoning, so the two are split rather
			// than merged into content.
			reasoning, answer := strip.pushSplit(frame.Content)
			if reasoning != "" {
				chunk := ChatCompletion{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []Choice{{Index: 0,
						Delta: &RespMsg{ReasoningContent: reasoning}, FinishReason: nil}},
				}
				if err := sw.event("", chunk); err != nil {
					stream.Close(err)
					return
				}
			}
			if answer == "" {
				continue
			}
			// The tool parser decides whether this reply is prose or a call.
			// It releases prose immediately and holds a possible call back
			// until the object is complete.
			visible := toolParser.observe(answer)
			if visible == "" {
				continue
			}
			chunk := ChatCompletion{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []Choice{{Index: 0,
					Delta: &RespMsg{Content: visible}, FinishReason: nil}},
			}
			if err := sw.event("", chunk); err != nil {
				stream.Close(err)
				return
			}
		case frameUsage:
			if frame.Usage != nil {
				u := &Usage{
					PromptTokens:     frame.Usage.PromptTokens,
					CompletionTokens: frame.Usage.CompletionTokens,
					TotalTokens:      frame.Usage.TotalTokens,
				}
				usage = u
			}
		case frameError:
			streamErr = fmt.Errorf("%s", frame.Content)
			finishReason = "error"
		case frameSensitiveQuery, frameSensitiveTitle:
			finishReason = "content_filter"
		case frameFinish:
			// The backend is done; the channel closes on its own.
		case frameDialogID, frameWebSearch, frameDoc, frameTipRatio, frameTipTruncate:
			// Metadata that has no OpenAI equivalent. Dropping it is correct:
			// inventing a field would break strict SDK parsers.
		}
	}

	// The stripper holds back a few trailing bytes in case they are the start
	// of a marker. Once the stream ends they are ordinary text and must be
	// released, or the last word of every reply is truncated.
	if tail := strip.flush(); tail != "" {
		if visible := toolParser.observe(tail); visible != "" {
			chunk := ChatCompletion{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []Choice{{Index: 0,
					Delta: &RespMsg{Content: visible}, FinishReason: nil}},
			}
			if err := sw.event("", chunk); err != nil {
				stream.Close(err)
				return
			}
		}
	}

	// Resolve the parser. Any buffered reply that is not a call is released as
	// text so the user still sees it.
	tailText, calls := toolParser.finish()
	if tailText != "" {
		chunk := ChatCompletion{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []Choice{{Index: 0,
				Delta: &RespMsg{Content: tailText}, FinishReason: nil}},
		}
		if err := sw.event("", chunk); err != nil {
			stream.Close(err)
			return
		}
	}

	if len(calls) > 0 {
		// A tool call is emitted the way OpenAI clients expect it: one chunk
		// opening the call by index with its id and name, then a chunk
		// carrying the arguments. Clients assemble them by index, so emitting
		// the whole call in one chunk also works, but this mirrors the
		// upstream shape exactly and no client has to special-case it.
		finishReason = "tool_calls"
		for i, c := range calls {
			idx := i
			open := ChatCompletion{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []Choice{{Index: 0, Delta: &RespMsg{
					ToolCalls: []ToolCallOut{{
						Index: &idx, ID: c.ID, Type: "function",
						Function: ToolCallFuncOut{Name: c.Name},
					}},
				}, FinishReason: nil}},
			}
			if err := sw.event("", open); err != nil {
				stream.Close(err)
				return
			}
			args := ChatCompletion{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []Choice{{Index: 0, Delta: &RespMsg{
					ToolCalls: []ToolCallOut{{
						Index: &idx, Function: ToolCallFuncOut{Arguments: c.Arguments},
					}},
				}, FinishReason: nil}},
			}
			if err := sw.event("", args); err != nil {
				stream.Close(err)
				return
			}
		}
	}

	final := ChatCompletion{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []Choice{{Index: 0, Delta: &RespMsg{}, FinishReason: &finishReason}},
	}
	if usage != nil {
		final.Usage = usage
	}
	_ = sw.event("", final)
	// The terminator must be the literal `[DONE]` token, not a JSON string.
	_ = sw.raw("[DONE]")

	s.recordUsage(ctx, usage)

	if streamErr != nil {
		// The stream is already committed, so the error can only be reported
		// in-band; the finish_reason above carries the failure.
		s.log.Warn("stream finished with upstream error", "error", streamErr)
		stream.Close(streamErr)
		return
	}
	stream.Close(nil)
}

// recordUsage bills a key's counters. It is a no-op for static client tokens,
// which have no quota.
func (s *Server) recordUsage(ctx context.Context, u *Usage) {
	if s.keys == nil || u == nil {
		return
	}
	id := identityFrom(ctx)
	if id == nil || id.key == nil {
		return
	}
	total := int64(u.TotalTokens)
	if total == 0 {
		total = int64(u.PromptTokens + u.CompletionTokens)
	}
	s.keys.RecordUsage(id.key.ID, total)
}

// collectOpenAIWithRetry buffers the reply, and retries once with a format
// correction when the model attempted a tool call but produced nothing the
// parser could read.
//
// Retrying here rather than upstream of the handler is deliberate: the retry
// only makes sense once the reply has been seen, and only the non-streaming
// path may hold a reply back. A streaming client has already rendered the
// tokens by the time a malformed call is detectable.
func (s *Server) collectOpenAIWithRetry(w http.ResponseWriter, ctx context.Context,
	stream *upstream.ChatStream, id, model string, req ChatCompletionRequest,
	defs []ToolDef, upReq upstream.ChatRequest) {

	raw, usage, finishReason, streamErr := drainOpenAI(stream)
	if streamErr != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed",
			streamErr.Error())
		return
	}

	_, text := splitThink(raw)
	calls, remaining := parseToolCalls(text, defs)

	if len(defs) > 0 && len(calls) == 0 && looksLikeFailedCall(text) {
		// Resend with the format restated. The original reply is discarded
		// rather than shown, because it is a failed attempt at a tool call and
		// presenting it as an answer is what makes an agent stall silently.
		retryReq := upReq
		retryReq.Query += toolRetryInstruction
		if second, err := s.client.Chat(ctx, retryReq); err == nil {
			raw2, usage2, finish2, err2 := drainOpenAI(second)
			if err2 == nil {
				_, text2 := splitThink(raw2)
				if calls2, remaining2 := parseToolCalls(text2, defs); len(calls2) > 0 {
					raw, usage, finishReason = raw2, usage2, finish2
					text, calls, remaining = text2, calls2, remaining2
				}
			}
		}
	}

	s.writeOpenAIResult(w, id, model, req, raw, text, remaining, calls, usage, finishReason)
}

// drainOpenAI consumes a stream into its raw text and metadata.
func drainOpenAI(stream *upstream.ChatStream) (string, *Usage, string, error) {
	var raw strings.Builder
	var usage *Usage
	finishReason := "stop"
	var streamErr error

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
			streamErr = fmt.Errorf("%s", frame.Content)
			finishReason = "error"
		case frameSensitiveQuery, frameSensitiveTitle:
			finishReason = "content_filter"
		case frameFinish:
		case frameDialogID, frameWebSearch, frameDoc, frameTipRatio, frameTipTruncate:
		}
	}
	stream.Close(streamErr)
	return raw.String(), usage, finishReason, streamErr
}

// collectOpenAI drains the stream into a single non-streaming response.
func (s *Server) collectOpenAI(w http.ResponseWriter, ctx context.Context,
	stream *upstream.ChatStream, id, model string, req ChatCompletionRequest, defs []ToolDef) {

	var raw strings.Builder
	var usage *Usage
	finishReason := "stop"
	var streamErr error

	for frame := range stream.Frames {
		switch frame.Event {
		case frameMessage:
			// Split after the whole stream has been read, not per frame: a
			// marker can straddle two frames, and splitting each one
			// independently leaves half a tag in the text.
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
			streamErr = fmt.Errorf("%s", frame.Content)
		case frameSensitiveQuery, frameSensitiveTitle:
			finishReason = "content_filter"
		}
	}

	if streamErr != nil {
		stream.Close(streamErr)
		if upstream.IsAuthError(streamErr) {
			writeError(w, http.StatusBadGateway, "upstream_error",
				"session_invalid", "upstream session rejected")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_failed",
			streamErr.Error())
		return
	}
	stream.Close(nil)
	s.recordUsage(ctx, usage)

	_, text := splitThink(raw.String())

	// The model answers in prose unless it wants a tool. When it wants one the
	// reply is an envelope, and that has to become a structured tool_calls
	// field rather than being passed through as text — a client that receives
	// the envelope as content simply prints it.
	calls, remaining := parseToolCalls(text, defs)

	s.writeOpenAIResult(w, id, model, req, raw.String(), text, remaining, calls, usage, finishReason)
}

// writeOpenAIResult assembles and emits the non-streaming response.
//
// It is separate from the collection loop because the retry path produces the
// same outputs by a different route and both must render identically.
func (s *Server) writeOpenAIResult(w http.ResponseWriter, id, model string,
	req ChatCompletionRequest, raw, text, remaining string, calls []ToolCall,
	usage *Usage, finishReason string) {

	thought, _ := splitThink(raw)
	if len(calls) > 0 {
		finishReason = "tool_calls"
		text = remaining
	}

	resp := ChatCompletion{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []Choice{{
			Index: 0,
			Message: &RespMsg{
				Role:             "assistant",
				Content:          text,
				ReasoningContent: thought,
				ToolCalls:        toToolCallOut(calls),
			},
			FinishReason: &finishReason,
		}},
		Usage: usage,
	}
	if usage == nil {
		// The backend may omit usage on some paths; approximate so clients
		// that bill or budget on these numbers still get something sane.
		approx := &Usage{
			PromptTokens:     approxTokens(summarize(req.Messages)),
			CompletionTokens: approxTokens(text),
		}
		approx.TotalTokens = approx.PromptTokens + approx.CompletionTokens
		resp.Usage = approx
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeUpstreamError maps an upstream failure onto an OpenAI-shaped error.
func (s *Server) writeUpstreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrNoSession):
		writeError(w, http.StatusServiceUnavailable, "upstream_error",
			"no_session", "no upstream session configured or all are cooling down")
	case upstream.IsBannedError(err):
		writeError(w, http.StatusBadGateway, "upstream_error",
			"account_banned", "the MiMo account for this session is blocked")
	case upstream.IsAuthError(err):
		writeError(w, http.StatusBadGateway, "upstream_error",
			"cookies_expired", "MiMo session cookies are missing or expired; re-export them")
	default:
		s.log.Error("upstream request failed", "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error",
			"upstream_failed", err.Error())
	}
}

// toTurns normalizes inbound messages into upstream turns.
func toTurns(msgs []ChatMessage, systemMode string) ([]upstream.Turn, string, error) {
	turns := make([]upstream.Turn, 0, len(msgs))
	var results []ToolResult
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			role = "system"
		case "user":
			role = "user"
		case "assistant":
			role = "assistant"
		case "tool", "function":
			// A tool result has no equivalent upstream. It is rendered as the
			// result of a named call so the model can connect it to what it
			// asked for; the plain text alone would be ambiguous when several
			// calls are in flight.
			text := m.TextContent()
			if strings.TrimSpace(text) == "" {
				continue
			}
			results = append(results, ToolResult{
				CallID:  m.ToolCallID,
				Name:    m.Name,
				Content: text,
			})
			continue
		default:
			role = "user"
		}

		text := m.TextContent()
		// An assistant turn that requested tools carries the calls in a
		// structured field and often no text at all. Recording what was
		// requested is what lets the following tool result make sense.
		if calls := parseAssistantToolCalls(m.ToolCalls); len(calls) > 0 {
			text = strings.TrimSpace(text + "\n" + renderRequestedCalls(calls))
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		turns = append(turns, upstream.Turn{Role: role, Content: text})
	}
	if len(turns) == 0 && len(results) == 0 {
		return nil, "", errors.New("no non-empty message content")
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
	// Tool results have nowhere to go in the web protocol but the prompt, so
	// they are appended to the final turn. Without this the model never sees
	// what its own tool call returned and simply calls it again.
	if block := renderToolResults(results); block != "" {
		finalQuery += block
	}
	return history, finalQuery, nil
}

// parseAssistantToolCalls decodes the tool_calls field of a client-sent
// assistant message. A malformed field is ignored rather than fatal: the
// surrounding conversation is still usable.
func parseAssistantToolCalls(raw json.RawMessage) []ToolCall {
	if len(raw) == 0 {
		return nil
	}
	var outs []ToolCallOut
	if err := json.Unmarshal(raw, &outs); err != nil {
		return nil
	}
	calls := make([]ToolCall, 0, len(outs))
	for _, o := range outs {
		calls = append(calls, ToolCall{
			ID:        o.ID,
			Name:      o.Function.Name,
			Arguments: o.Function.Arguments,
		})
	}
	return calls
}

// renderRequestedCalls restates a previous tool request as text.
func renderRequestedCalls(calls []ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[called tools: ")
	for i, c := range calls {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteString("(")
		b.WriteString(c.Arguments)
		b.WriteString(")")
	}
	b.WriteString("]")
	return b.String()
}

func summarize(msgs []ChatMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.TextContent())
		b.WriteByte('\n')
	}
	return b.String()
}

// toToolCallOut renders parsed calls in the wire shape. It returns nil rather
// than an empty slice so the field is omitted entirely for an ordinary reply.
func toToolCallOut(calls []ToolCall) []ToolCallOut {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCallOut, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolCallOut{
			ID:       c.ID,
			Type:     "function",
			Function: ToolCallFuncOut{Name: c.Name, Arguments: c.Arguments},
		})
	}
	return out
}

// approxTokens is a coarse character-based estimate. It exists only to fill a
// missing usage field, never to override a real number from upstream.
func approxTokens(s string) int {
	if s == "" {
		return 0
	}
	// CJK text is roughly one token per character; latin text roughly one per
	// four. Counting runes and blending keeps the two from being wildly off.
	runes := 0
	for range s {
		runes++
	}
	return (runes + 1) / 2
}
