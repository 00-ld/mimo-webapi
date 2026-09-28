// Command mimowebapi exposes the MiMo Studio *web* backend as a local
// OpenAI- and Anthropic-compatible API.
//
// It does not use the MiMo developer API. It drives the same endpoints the
// browser UI uses, authenticated with a logged-in session's cookies.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mimowebapi/internal/apikeys"
	"mimowebapi/internal/config"
	"mimowebapi/internal/httpapi"
	"mimowebapi/internal/session"
	"mimowebapi/internal/upstream"
)

// version is reported by /healthz.
const version = "2.0.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "config.json", "path to config file")
		checkOnly   = flag.Bool("check", false, "validate config and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
		listModels  = flag.Bool("models", false, "print the configured model list and exit")
		healthcheck = flag.Bool("healthcheck", false,
			"probe the local /healthz endpoint and exit non-zero if it is not serving")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("mimowebapi", version)
		return nil
	}

	// A scratch container has no shell and no curl, so the readiness probe has
	// to be this binary. It exits non-zero when the local listener is not
	// answering, which is what a container health check reads.
	if *healthcheck {
		return probeHealth(*configPath)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *checkOnly {
		fmt.Printf("config OK: listen=%s sessions=%d models=%d\n",
			cfg.Listen, len(cfg.Upstream.Sessions), len(cfg.Models.List))
		return nil
	}
	if *listModels {
		fmt.Printf("default: %s\n", cfg.Models.Default)
		for _, m := range cfg.Models.List {
			fmt.Println(" ", m)
		}
		return nil
	}

	log := newLogger(cfg.Log.Level)

	cooldown := time.Duration(cfg.Upstream.CooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	// A previous authorisation is remembered on disk so restarting does not
	// force another login. Config-declared sessions win on label conflicts.
	sessions := append([]config.Session{}, cfg.Upstream.Sessions...)
	if extra, err := httpapi.LoadPersistedSessions(cfg.SessionFile()); err != nil {
		log.Warn("could not read saved sessions", "error", err)
	} else {
		seen := map[string]bool{}
		for _, s := range sessions {
			seen[s.Label] = true
		}
		for _, s := range extra {
			if !seen[s.Label] {
				sessions = append(sessions, s)
			}
		}
		if len(extra) > 0 {
			log.Info("loaded saved sessions", "count", len(extra))
		}
	}

	pool := session.New(sessions, cooldown)
	client, err := upstream.New(cfg, pool)
	if err != nil {
		return err
	}

	keyStore, err := apikeys.Open(cfg.Admin.KeyStorePath)
	if err != nil {
		return err
	}
	// Usage counters change on every request, so flush periodically rather
	// than on each one.
	stopFlush := startKeyFlusher(keyStore, log)
	defer stopFlush()

	server := httpapi.NewServer(cfg, client, log, keyStore)

	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: withRequestLog(server.Routes(), log),
		// No write timeout: a streaming completion can legitimately outlive
		// any fixed deadline. Read timeouts still bound a stalled upload.
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	if len(sessions) == 0 {
		log.Warn("no account authorised yet: open /console and use the " +
			"authorisation wizard, or set upstream.sessions in the config")
	}
	if cfg.Admin.Password == "" {
		log.Info("admin console disabled (admin.password not set); " +
			"only static client_tokens are accepted")
	} else {
		log.Info("admin console enabled", "path", "/console",
			"keys_issued", keyStore.Count())
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "sessions", pool.Size(),
			"models", len(cfg.Models.List), "version", version)
		if err := httpSrv.ListenAndServe(); err != nil &&
			err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// startKeyFlusher persists the key store on a timer and on shutdown.
func startKeyFlusher(store *apikeys.Store, log *slog.Logger) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				if err := store.Flush(); err != nil {
					log.Error("final key store flush failed", "error", err)
				}
				return
			case <-t.C:
				if err := store.Flush(); err != nil {
					log.Error("key store flush failed", "error", err)
				}
			}
		}
	}()
	return func() { close(done) }
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush keeps streaming working through the wrapper. Without it the relay
// would silently buffer every SSE response.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withRequestLog(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

// probeHealth asks the local listener whether it is serving.
//
// The address comes from the config so the probe follows the same listen value
// the server was started with, including a non-default port.
func probeHealth(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return err
	}
	// A wildcard bind is not a dialable address; the loopback interface is.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: status %d", resp.StatusCode)
	}
	return nil
}
