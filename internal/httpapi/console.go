package httpapi

import (
	_ "embed"
	"net/http"
)

// consoleHTML is the built-in admin console, embedded so the binary stays a
// single file with no external assets to deploy.
//
//go:embed console.html
var consoleHTML []byte

// handleConsole serves the console page.
//
// The page itself carries no secrets — it is a shell that authenticates
// against /admin/api/login and then talks to the management API with a signed
// session cookie. Serving it unauthenticated is therefore safe, and it lets
// the login form itself be part of the app.
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/console" && r.URL.Path != "/console/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The console is same-origin only; lock it down so an injected page cannot
	// talk to the admin API from elsewhere.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
			"connect-src 'self'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(consoleHTML)
}
