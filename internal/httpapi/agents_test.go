package httpapi

// Tests for the one-click onboarding in agents.go.
//
// Every test builds its own agentEnv rooted at a t.TempDir(). Nothing here may
// touch a real user's configuration: the whole point of injecting the root
// directories is that a test can never reach $HOME by accident.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// testRelay is the relay the tests pretend to be.
	testRelay = "http://127.0.0.1:8793"
	// agentToken is the credential written into every config. It is named
	// apart from testutil_test.go's agentToken, which is an upstream credential.
	agentToken = "sk-mimo-0123456789abcdef0123456789abcdef"
)

// tempEnv builds an isolated agentEnv rooted at a fresh tempdir.
func tempEnv(t *testing.T) (agentEnv, string) {
	t.Helper()
	home := t.TempDir()
	return agentEnv{home: home, vars: map[string]string{}}, home
}

// writeFile creates a file (and its parents) with the given content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readFile reads a file or fails the test.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// managedBlock returns the text between the onboarding markers.
func managedBlock(t *testing.T, content string) string {
	t.Helper()
	start := strings.Index(content, dshAnchorBegin)
	end := strings.Index(content, dshAnchorEnd)
	if start < 0 || end < 0 || end < start {
		t.Fatalf("no managed block in:\n%s", content)
	}
	return content[start : end+len(dshAnchorEnd)]
}

// countBackups lists the backups made beside a file.
func countBackups(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// ---- Codex -----------------------------------------------------------------

const codexSample = `model_provider = "custom"
model = "mimo-v2.6-pro"

notify = ["/usr/local/bin/notify", "turn-ended"]
web_search = "disabled"

[model_providers.custom]
name = "custom"
wire_api = "responses"
requires_openai_auth = false
base_url = "http://127.0.0.1:8793"
experimental_bearer_token = "old-token"

[mcp_servers]

[mcp_servers.node_repl]
type = "stdio"
command = "/usr/bin/node_repl"
startup_timeout_sec = 120

[projects."/Users/someone/work"]
trust_level = "trusted"
`

func TestConfigureCodexPreservesUnrelatedContent(t *testing.T) {
	e, home := tempEnv(t)
	path := filepath.Join(home, ".codex", "config.toml")
	writeFile(t, path, codexSample)

	res, err := configureAgent(e, agentCodex, testRelay, agentToken)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if !res.Changed || res.BackupPath == "" {
		t.Fatalf("expected a change with a backup, got %+v", res)
	}

	got := readFile(t, path)
	for _, want := range []string{
		`experimental_bearer_token = "` + agentToken + `"`,
		`base_url = "http://127.0.0.1:8793"`,
		`wire_api = "responses"`,
		`requires_openai_auth = false`,
		`model_provider = "custom"`,
		`model = "mimo-v2.6-pro"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten config missing %q\n%s", want, got)
		}
	}
	// Everything unrelated must survive byte for byte.
	for _, want := range []string{
		`notify = ["/usr/local/bin/notify", "turn-ended"]`,
		`web_search = "disabled"`,
		`[mcp_servers.node_repl]`,
		`command = "/usr/bin/node_repl"`,
		`startup_timeout_sec = 120`,
		`[projects."/Users/someone/work"]`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten config dropped %q\n%s", want, got)
		}
	}

	// The backup must hold the original bytes exactly.
	backup := readFile(t, res.BackupPath)
	if backup != codexSample {
		t.Errorf("backup is not the original:\n%q\nvs\n%q", backup, codexSample)
	}
}

func TestConfigureCodexIsIdempotent(t *testing.T) {
	e, home := tempEnv(t)
	path := filepath.Join(home, ".codex", "config.toml")
	writeFile(t, path, codexSample)

	first, err := configureAgent(e, agentCodex, testRelay, agentToken)
	if err != nil {
		t.Fatalf("first configure: %v", err)
	}
	afterFirst := readFile(t, path)

	second, err := configureAgent(e, agentCodex, testRelay, agentToken)
	if err != nil && err != errUnchanged {
		t.Fatalf("second configure: %v", err)
	}
	if second.Changed {
		t.Error("second configure reported a change; the write is not idempotent")
	}
	if got := readFile(t, path); got != afterFirst {
		t.Errorf("second configure changed the file:\n%s\nvs\n%s", got, afterFirst)
	}
	// Exactly one backup: the no-op run must not stack another.
	if backups := countBackups(t, path); len(backups) != 1 {
		t.Errorf("backup count = %d, want 1 (%v)", len(backups), backups)
	}
	if first.BackupPath == second.BackupPath {
		t.Error("the no-op run reused the first run's backup path")
	}
}

func TestConfigureCodexCreatesMissingProviderSection(t *testing.T) {
	e, home := tempEnv(t)
	path := filepath.Join(home, ".codex", "config.toml")
	writeFile(t, path, "model = \"gpt-5\"\n\n[history]\npersistence = \"none\"\n")

	if _, err := configureAgent(e, agentCodex, testRelay, agentToken); err != nil {
		t.Fatalf("configure: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "[model_providers.custom]") {
		t.Errorf("provider table was not created:\n%s", got)
	}
	if !strings.Contains(got, `model = "gpt-5"`) {
		t.Errorf("the user's chosen model was overwritten:\n%s", got)
	}
	if !strings.Contains(got, `[history]`) {
		t.Errorf("unrelated table was dropped:\n%s", got)
	}
}

func TestConfigureCodexRejectsMalformedTOML(t *testing.T) {
	e, home := tempEnv(t)
	path := filepath.Join(home, ".codex", "config.toml")
	// A top-level line that is neither a header, a key nor a comment means the
	// writer cannot trust its anchors.
	bad := "model_provider = \"custom\"\nthis line is not TOML at all\n"
	writeFile(t, path, bad)

	if _, err := configureAgent(e, agentCodex, testRelay, agentToken); err == nil {
		t.Fatal("expected malformed TOML to be rejected")
	}
	_ = bad
	// The poisoned input must be left exactly as it was, with no backup taken.
	if got := readFile(t, path); got != bad {
		t.Errorf("file was modified:\n%q", got)
	}
	if backups := countBackups(t, path); len(backups) != 0 {
		t.Errorf("a rejected write still took a backup: %v", backups)
	}
}

// ---- Claude Code -----------------------------------------------------------

const claudeSample = `{
  "enabledPlugins": {
    "code-review@claude-plugins-official": true
  },
  "env": {
    "ANTHROPIC_AUTH_TOKEN": "old-token",
    "ANTHROPIC_BASE_URL": "https://api.example.com",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "some-model",
    "KEEP_ME": "1"
  },
  "extraKnownMarketplaces": {
    "claude-hud": {
      "source": {
        "repo": "jarrodwatts/claude-hud",
        "source": "github"
      }
    }
  }
}
`

func TestConfigureClaudePreservesUnrelatedKeys(t *testing.T) {
	e, home := tempEnv(t)
	path := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, path, claudeSample)

	res, err := configureAgent(e, agentClaude, testRelay, agentToken)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if !res.Changed {
		t.Error("expected a change")
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	env, ok := doc["env"].(map[string]any)
	if !ok {
		t.Fatal("env block missing")
	}
	if env["ANTHROPIC_BASE_URL"] != testRelay {
		t.Errorf("base url = %v", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != agentToken {
		t.Errorf("token = %v", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if env["KEEP_ME"] != "1" {
		t.Error("an unrelated env key was dropped")
	}
	// Unrelated top-level keys survive.
	if _, ok := doc["enabledPlugins"]; !ok {
		t.Error("enabledPlugins was dropped")
	}
	if _, ok := doc["extraKnownMarketplaces"]; !ok {
		t.Error("extraKnownMarketplaces was dropped")
	}
	// The model is rewritten to the relay's own, even though the fixture holds
	// a different name. A leftover name from the previous gateway is unusable
	// once the base URL has moved: requests would fail with
	// unrecognized_model. Base URL and model name are one decision.
	if env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != defaultAgentModel {
		t.Errorf("model = %v, want %q (the relay's own)",
			env["ANTHROPIC_DEFAULT_OPUS_MODEL"], defaultAgentModel)
	}
	// The *_NAME siblings must follow, or the Claude Code UI reports a model
	// the relay does not serve.
	if env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"] != defaultAgentModel {
		t.Errorf("model name sibling = %v, want %q",
			env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"], defaultAgentModel)
	}
	// Every model key the relay does own carries the relay's model.
	for _, key := range claudeModelKeys {
		if env[key] != defaultAgentModel {
			t.Errorf("model key %s = %v, want %q", key, env[key], defaultAgentModel)
		}
	}

	// Key order is preserved, which matters because this file is hand-edited.
	got := readFile(t, path)
	if strings.Index(got, "enabledPlugins") > strings.Index(got, "\"env\"") {
		t.Errorf("top-level key order changed:\n%s", got)
	}
}

func TestConfigureClaudeCreatesEnvBlock(t *testing.T) {
	e, home := tempEnv(t)
	path := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, path, `{"enabledPlugins": {}}`)

	if _, err := configureAgent(e, agentClaude, testRelay, agentToken); err != nil {
		t.Fatalf("configure: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	env, ok := doc["env"].(map[string]any)
	if !ok {
		t.Fatal("env block was not created")
	}
	if env["ANTHROPIC_BASE_URL"] != testRelay {
		t.Errorf("base url = %v", env["ANTHROPIC_BASE_URL"])
	}
	// A fresh install gets the relay's default model.
	if env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != defaultAgentModel {
		t.Errorf("model = %v, want %v", env["ANTHROPIC_DEFAULT_OPUS_MODEL"], defaultAgentModel)
	}
}

func TestConfigureClaudeRejectsNonObjectEnv(t *testing.T) {
	e, home := tempEnv(t)
	path := filepath.Join(home, ".claude", "settings.json")
	bad := `{"env": "not-an-object"}`
	writeFile(t, path, bad)

	if _, err := configureAgent(e, agentClaude, testRelay, agentToken); err == nil {
		t.Fatal("expected a non-object env to be rejected")
	}
	if got := readFile(t, path); got != bad {
		t.Errorf("file was modified:\n%s", got)
	}
}

func TestConfigureClaudeRejectsBrokenJSON(t *testing.T) {
	e, home := tempEnv(t)
	path := filepath.Join(home, ".claude", "settings.json")
	bad := `{"env": {"ANTHROPIC_BASE_URL": }`
	writeFile(t, path, bad)

	if _, err := configureAgent(e, agentClaude, testRelay, agentToken); err == nil {
		t.Fatal("expected broken JSON to be rejected")
	}
	if got := readFile(t, path); got != bad {
		t.Errorf("file was modified:\n%s", got)
	}
}

// ---- DeepSeek Harness ------------------------------------------------------

const dshSample = `# Your patch layer for this dsh profile.

- insert:
    - id: dsh-infinite-gen-4
      name: 'dsh-infinite-gen-4'

- id: web
  name: '@deepseek-ai/dsh-web'
  config:
    searchProvider: tavily

- id: llm-pi-ai
  name: "@deepseek-ai/dsh-llm-pi-ai"
  config:
    providers:
      wp:
        apiKeyEnv: WP_API_KEY
        api: openai-completions
        baseURL: https://api.example.com/v1
        models:
          - id: some-model
            name: some-model

- id: agent-default-model
  name: "@deepseek-ai/dsh-agent-default-model"
  config:
    provider: wp
    model: some-model
`

func dshPath(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(home, ".dsh", "profiles", "web", "cordis.patch.yml")
}

func TestConfigureDshPreservesUnrelatedEntries(t *testing.T) {
	e, home := tempEnv(t)
	path := dshPath(t, home)
	writeFile(t, path, dshSample)

	res, err := configureAgent(e, agentDsh, testRelay, agentToken)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if !res.Changed {
		t.Error("expected a change")
	}

	got := readFile(t, path)
	for _, want := range []string{
		"- id: dsh-infinite-gen-4",
		"- id: web",
		"searchProvider: tavily",
		"- id: llm-pi-ai",
		"apiKeyEnv: WP_API_KEY",
		"api: openai-completions",
		"baseURL: https://api.example.com/v1",
		"- id: agent-default-model",
		"provider: wp",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten patch layer dropped %q\n%s", want, got)
		}
	}
	// Each protocol gets its own provider, since one provider speaks one api.
	// The assertions are scoped to the managed block: the sample file contains
	// "openai-completions" of its own, and counting the whole document would
	// measure the fixture rather than the write.
	block := managedBlock(t, got)
	for _, api := range []string{"openai-completions", "openai-responses", "anthropic-messages"} {
		if n := strings.Count(block, "api: "+api+"\n"); n != 1 {
			t.Errorf("expected exactly one provider with api %s in the managed block, got %d\n%s",
				api, n, block)
		}
	}
	if n := strings.Count(block, "apiKeyEnv: MIMO_API_KEY"); n != 3 {
		t.Errorf("MIMO_API_KEY appears %d times, want 3\n%s", n, block)
	}
	// The inserted block must land inside the llm-pi-ai entry, before it, at
	// the same indentation as a sibling.
	idxBlock := strings.Index(got, "- id: "+dshProviderName)
	idxEntry := strings.Index(got, "- id: llm-pi-ai")
	if idxBlock < 0 || idxEntry < 0 || idxBlock > idxEntry {
		t.Errorf("managed block is not before the llm-pi-ai entry (block=%d entry=%d)",
			idxBlock, idxEntry)
	}
	// The result must be parseable and its top level a sequence.
	if !topLevelIsSequence(got) {
		t.Error("rewritten file is no longer a top-level YAML sequence")
	}
	if _, err := parseYAMLBlocks(got); err != nil {
		t.Errorf("rewritten file does not parse: %v", err)
	}
}

func TestConfigureDshIsIdempotent(t *testing.T) {
	e, home := tempEnv(t)
	path := dshPath(t, home)
	writeFile(t, path, dshSample)

	if _, err := configureAgent(e, agentDsh, testRelay, agentToken); err != nil {
		t.Fatalf("first configure: %v", err)
	}
	afterFirst := readFile(t, path)

	second, err := configureAgent(e, agentDsh, testRelay, agentToken)
	if err != nil && err != errUnchanged {
		t.Fatalf("second configure: %v", err)
	}
	if second.Changed {
		t.Error("second configure reported a change")
	}
	if got := readFile(t, path); got != afterFirst {
		t.Errorf("second configure changed the file:\n%s\nvs\n%s", got, afterFirst)
	}
	// One managed block, not three stacked ones.
	if n := strings.Count(afterFirst, dshAnchorBegin); n != 1 {
		t.Errorf("managed block appears %d times, want 1", n)
	}
	// The managed block is counted once; the user's own wp provider is not
	// inside it, so the block holds exactly the three providers we wrote.
	block := managedBlock(t, afterFirst)
	if n := strings.Count(block, "        api: "); n != 3 {
		t.Errorf("managed block holds %d providers, want 3\n%s", n, block)
	}
}

func TestConfigureDshAppendsWhenAnchorMissing(t *testing.T) {
	e, home := tempEnv(t)
	path := dshPath(t, home)
	base := "- id: web\n  name: '@deepseek-ai/dsh-web'\n"
	writeFile(t, path, base)

	if _, err := configureAgent(e, agentDsh, testRelay, agentToken); err != nil {
		t.Fatalf("configure: %v", err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "- id: web") {
		t.Errorf("existing entry was dropped:\n%s", got)
	}
	if !strings.Contains(got, "name: '@deepseek-ai/dsh-llm-pi-ai'") {
		t.Errorf("a self-contained provider entry was not written:\n%s", got)
	}
	if _, err := parseYAMLBlocks(got); err != nil {
		t.Errorf("rewritten file does not parse: %v", err)
	}
}

func TestConfigureDshRepairsTruncatedBlock(t *testing.T) {
	e, home := tempEnv(t)
	path := dshPath(t, home)
	// A previous run was cut off after the opening marker: everything from the
	// marker on is treated as the managed region and replaced.
	truncated := dshSample + "\n" + dshAnchorBegin + "\n- id: " + dshProviderName + "\n"
	writeFile(t, path, truncated)

	if _, err := configureAgent(e, agentDsh, testRelay, agentToken); err != nil {
		t.Fatalf("configure: %v", err)
	}
	got := readFile(t, path)
	if n := strings.Count(got, dshAnchorBegin); n != 1 {
		t.Errorf("marker count = %d, want 1", n)
	}
	if n := strings.Count(got, dshAnchorEnd); n != 1 {
		t.Errorf("closing marker count = %d, want 1", n)
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), dshAnchorEnd) {
		t.Errorf("block does not end at the closing marker:\n%s", got)
	}
}

func TestConfigureDshRejectsNonSequence(t *testing.T) {
	e, home := tempEnv(t)
	path := dshPath(t, home)
	bad := "id: llm-pi-ai\nconfig:\n  providers: {}\n"
	writeFile(t, path, bad)

	if _, err := configureAgent(e, agentDsh, testRelay, agentToken); err == nil {
		t.Fatal("expected a non-sequence document to be rejected")
	}
	if got := readFile(t, path); got != bad {
		t.Errorf("file was modified:\n%s", got)
	}
	if backups := countBackups(t, path); len(backups) != 0 {
		t.Errorf("a rejected write still took a backup: %v", backups)
	}
}

func TestConfigureDshHonoursProfileEnv(t *testing.T) {
	home := t.TempDir()
	e := agentEnv{home: home, vars: map[string]string{"DSH_PROFILE": "headless"}}
	path := filepath.Join(home, ".dsh", "profiles", "headless", "cordis.patch.yml")
	writeFile(t, path, "- id: web\n  name: '@deepseek-ai/dsh-web'\n")

	if _, err := configureAgent(e, agentDsh, testRelay, agentToken); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if !strings.Contains(readFile(t, path), dshAnchorBegin) {
		t.Error("the profile from DSH_PROFILE was not targeted")
	}
}

func TestSanitizeProfileRejectsTraversal(t *testing.T) {
	for _, in := range []string{"../../etc", "..", ".", "web/../../x", ""} {
		got := sanitizeProfile(in)
		if strings.Contains(got, "/") || strings.Contains(got, "..") || got == "" {
			t.Errorf("sanitizeProfile(%q) = %q, want a single safe path element", in, got)
		}
	}
}

// ---- detection -------------------------------------------------------------

func TestListAgentsDetectsInstalledOnes(t *testing.T) {
	e, home := tempEnv(t)
	writeFile(t, filepath.Join(home, ".codex", "config.toml"), codexSample)
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), claudeSample)
	if err := os.MkdirAll(filepath.Join(home, "Library", "Application Support", "ZCode"), 0o755); err != nil {
		t.Fatal(err)
	}

	rows := listAgents(e, testRelay, agentToken)
	byID := map[string]agentStatus{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if len(rows) != 5 {
		t.Errorf("agent count = %d, want 5", len(rows))
	}
	for _, id := range []string{agentCodex, agentClaude, agentZCode} {
		if !byID[id].Detected {
			t.Errorf("%s should be detected", id)
		}
		if byID[id].DetectedPath == "" {
			t.Errorf("%s has no detected path", id)
		}
	}
	for _, id := range []string{agentDsh, agentWorkBuddy} {
		if byID[id].Detected {
			t.Errorf("%s should not be detected in an empty home", id)
		}
	}
	// WorkBuddy stays read-only: its configuration schema is not confirmed, so
	// its entry must say so and still carry manual steps.
	for _, id := range []string{agentWorkBuddy} {
		if byID[id].SupportsWrite {
			t.Errorf("%s must not claim write support", id)
		}
		if byID[id].Reason == "" {
			t.Errorf("%s has no reason for being read-only", id)
		}
		if len(byID[id].ManualSteps) == 0 {
			t.Errorf("%s has no manual steps", id)
		}
	}
	// ZCode is writable: its provider list is a plain JSON document whose
	// schema has been confirmed. It must still carry manual steps for the
	// case where the user prefers the in-app settings screen.
	if !byID[agentZCode].SupportsWrite {
		t.Error("zcode must claim write support")
	}
	if len(byID[agentZCode].ManualSteps) == 0 {
		t.Error("zcode has no manual steps")
	}
	// Every agent offers manual steps as the fallback path.
	for _, r := range rows {
		if len(r.ManualSteps) == 0 {
			t.Errorf("%s: no manual steps", r.ID)
		}
	}
}

func TestListAgentsReportsConfiguredState(t *testing.T) {
	e, home := tempEnv(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	writeFile(t, codexPath, codexSample)

	// Before: a provider section exists but points elsewhere.
	rows := listAgents(e, "http://127.0.0.1:9999", agentToken)
	for _, r := range rows {
		if r.ID == agentCodex && r.Configured {
			t.Error("codex should not be configured for a different relay")
		}
	}
	// After: same relay, so configured flips true.
	rows = listAgents(e, testRelay, agentToken)
	for _, r := range rows {
		if r.ID == agentCodex && !r.Configured {
			t.Error("codex should be reported as configured")
		}
	}
}

func TestConfigureIsRejectedWithoutToken(t *testing.T) {
	e, home := tempEnv(t)
	path := filepath.Join(home, ".codex", "config.toml")
	writeFile(t, path, codexSample)

	if _, err := configureAgent(e, agentCodex, testRelay, ""); err == nil {
		t.Fatal("expected an empty token to be refused")
	}
	if got := readFile(t, path); got != codexSample {
		t.Errorf("file was modified:\n%s", got)
	}
	if backups := countBackups(t, path); len(backups) != 0 {
		t.Errorf("a refused write still took a backup: %v", backups)
	}
}

func TestConfigureUnknownAgent(t *testing.T) {
	e, _ := tempEnv(t)
	if _, err := configureAgent(e, "nope", testRelay, agentToken); err == nil {
		t.Fatal("expected an unknown agent id to be rejected")
	}
}

func TestConfigureRefusesReadOnlyAgent(t *testing.T) {
	e, _ := tempEnv(t)
	// WorkBuddy is the remaining read-only agent: no confirmed schema, so a
	// write against it must be refused rather than guessed.
	if _, err := configureAgent(e, agentWorkBuddy, testRelay, agentToken); err == nil {
		t.Fatal("expected a write to a read-only agent to be refused")
	}
}

// ---- concurrency and safety ------------------------------------------------

func TestBackupsDoNotCollideWithinASecond(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeFile(t, path, "first\n")

	first, err := backupFile(path, []byte("first\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := backupFile(path, []byte("second\n"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two backups in the same second share a path: %s", first)
	}
	if got := readFile(t, first); got != "first\n" {
		t.Errorf("first backup was clobbered: %q", got)
	}
	if got := readFile(t, second); got != "second\n" {
		t.Errorf("second backup content = %q", got)
	}
}

func TestHostOnly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:8793", "127.0.0.1:8793"},
		{"http://127.0.0.1:8793/", "127.0.0.1:8793"},
		{"https://relay.example.com", "relay.example.com"},
		{"http://127.0.0.1:8793/v1", "127.0.0.1:8793"},
		{"", ""},
	} {
		if got := hostOnly(tc.in); got != tc.want {
			t.Errorf("hostOnly(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRelayURLNormalisesWildcardListen(t *testing.T) {
	cfg := testConfig("https://example.invalid")
	cfg.Admin.Password = adminPW
	cfg.Listen = "0.0.0.0:8793"
	s, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.relayURLForAgents(); got != testRelay {
		t.Errorf("relay url = %q, want %q", got, testRelay)
	}
}

// ---- HTTP surface ----------------------------------------------------------

func TestAgentsEndpointRequiresAuth(t *testing.T) {
	s := adminServer(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/api/agents"},
		{"POST", "/admin/api/agents/codex/configure"},
	} {
		rec := doJSON(t, s.Routes(), tc.method, tc.path, "")
		if rec.Code != 401 {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAgentsEndpointListsAllAgents(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodGet, "/admin/api/agents", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Agents []agentStatus `json:"agents"`
		Model  string        `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Agents) != 5 {
		t.Errorf("agent count = %d, want 5", len(body.Agents))
	}
	if body.Model == "" {
		t.Error("model is empty")
	}
	for _, a := range body.Agents {
		if a.ID == "" || a.Name == "" {
			t.Errorf("incomplete row: %+v", a)
		}
	}
}

func TestAgentConfigureEndpointRejectsUnknownID(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodPost, "/admin/api/agents/nope/configure", "")
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAgentConfigureEndpointRejectsGet(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodGet, "/admin/api/agents/codex/configure", "")
	if rec.Code != 405 {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestAgentsEndpointsEndToEnd walks exactly the two calls the console makes,
// against a tempdir-backed home, and proves the HTTP layer writes nothing
// outside it.
func TestAgentsEndpointsEndToEnd(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".codex", "config.toml"), codexSample)

	cfg := testConfig("https://example.invalid")
	cfg.Admin.Password = adminPW
	cfg.ClientTokens = []string{agentToken}
	s, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.agentsEnvOverride = &agentEnv{home: home, vars: map[string]string{}}
	routes := s.Routes()

	rec := adminCall(t, routes, http.MethodGet, "/admin/api/agents", "")
	if rec.Code != 200 {
		t.Fatalf("list status = %d body = %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Agents []agentStatus `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var codex *agentStatus
	for i := range list.Agents {
		if list.Agents[i].ID == agentCodex {
			codex = &list.Agents[i]
		}
	}
	if codex == nil {
		t.Fatal("codex row missing")
	}
	if !codex.Detected {
		t.Error("codex was not detected in the tempdir home")
	}
	if codex.ConfigPath != filepath.Join(home, ".codex", "config.toml") {
		t.Errorf("config path = %q, want the tempdir path", codex.ConfigPath)
	}
	if !strings.HasPrefix(codex.ConfigPath, home) {
		t.Fatalf("the endpoint resolved a path outside the tempdir: %q", codex.ConfigPath)
	}
	if codex.Configured {
		t.Error("codex should not be configured for the test relay yet")
	}

	rec = adminCall(t, routes, http.MethodPost, "/admin/api/agents/codex/configure", "")
	if rec.Code != 200 {
		t.Fatalf("configure status = %d body = %s", rec.Code, rec.Body.String())
	}
	var res configureResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode configure: %v", err)
	}
	if !res.OK || !res.Changed {
		t.Fatalf("configure result = %+v", res)
	}
	if !strings.HasPrefix(res.BackupPath, home) {
		t.Fatalf("backup landed outside the tempdir: %q", res.BackupPath)
	}
	if _, err := os.Stat(res.BackupPath); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}

	// The token from the config was installed, and the relay URL is loopback.
	got := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(got, agentToken) {
		t.Errorf("token was not written:\n%s", got)
	}
	// The relay URL is derived from the configured listen address, which the
	// test fixture leaves as 127.0.0.1:0, so assert the derivation rather than
	// the constant.
	if want := s.relayURLForAgents(); !strings.Contains(got, want) {
		t.Errorf("relay url %q was not written:\n%s", want, got)
	}

	// A second call is a no-op and reports so.
	rec = adminCall(t, routes, http.MethodPost, "/admin/api/agents/codex/configure", "")
	if rec.Code != 200 {
		t.Fatalf("second configure status = %d body = %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode second configure: %v", err)
	}
	if res.Changed {
		t.Error("second configure reported a change")
	}
	if n := len(countBackups(t, filepath.Join(home, ".codex", "config.toml"))); n != 1 {
		t.Errorf("backup count = %d, want 1", n)
	}

	// The status call now reports the agent as configured.
	rec = adminCall(t, routes, http.MethodGet, "/admin/api/agents", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, a := range list.Agents {
		if a.ID == agentCodex && !a.Configured {
			t.Error("codex should be reported as configured after the write")
		}
	}
}

// TestAgentConfigureEndpointRefusesWithoutKey proves the endpoint reports a
// conflict, rather than writing a config with an empty token, when no key has
// been issued and no client token is configured.
func TestAgentConfigureEndpointRefusesWithoutKey(t *testing.T) {
	cfg := testConfig("https://example.invalid")
	cfg.Admin.Password = adminPW
	cfg.ClientTokens = nil
	s, err := buildServerNoSessions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := adminCall(t, s.Routes(), http.MethodPost, "/admin/api/agents/codex/configure", "")
	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	var res configureResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.OK {
		t.Error("reported ok on a refused write")
	}
	if res.Message == "" {
		t.Error("no explanation for the refusal")
	}
}
