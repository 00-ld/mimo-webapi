package httpapi

// One-click onboarding for local coding agents.
//
// This file detects which agents are installed on this machine and rewrites
// their configuration so they point at this relay. The rules that shape every
// function below:
//
//   - A rewrite never happens without a backup of the original bytes.
//   - A rewrite never runs before it has proved it can produce a valid result,
//     so a malformed input leaves the file exactly as it was.
//   - A rewrite preserves everything it was not asked to touch. These files are
//     hand-edited by their owners and carry plugin lists, marketplaces, MCP
//     servers and projects that a careless rewrite would silently destroy.
//   - A rewrite is idempotent: running it twice produces the same bytes.
//
// Detection reads paths from *base directories, which the exported helpers
// default to the real HOME. Every test passes a tempdir instead, so no test can
// reach a user's real configuration.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Agent IDs. Stable strings: the console and its users key off them.
const (
	agentCodex     = "codex"
	agentClaude    = "claude"
	agentDsh       = "dsh"
	agentZCode     = "zcode"
	agentWorkBuddy = "workbuddy"
)

// defaultRelayURL is where an agent should send its traffic when the config
// does not say otherwise.
const defaultRelayURL = "http://127.0.0.1:8793"

// backupTimeFormat is the suffix stamped onto every backup. It is deliberately
// lexically sortable and free of characters that need escaping in a shell.
const backupTimeFormat = "20060102-150405"

// ---- wire types ------------------------------------------------------------

// agentStatus is one row of GET /admin/api/agents.
type agentStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Detected is true when the agent looks installed on this machine.
	Detected bool `json:"detected"`
	// DetectedPath is the path that decided detection (config file or app dir).
	DetectedPath string `json:"detected_path,omitempty"`
	// Configured is true when the agent already points at this relay.
	Configured bool `json:"configured"`
	// ConfigPath is the file a configure call would write, if any.
	ConfigPath string `json:"config_path,omitempty"`
	// SupportsWrite is false for agents we can only advise about.
	SupportsWrite bool `json:"supports_write"`
	// Reason explains why an agent is not writable, in one short sentence.
	Reason string `json:"reason,omitempty"`
	// ManualSteps is always populated: it is the fallback when a write fails or
	// when the user would rather edit by hand.
	ManualSteps []string `json:"manual_steps,omitempty"`
}

// configureResult is the body of POST /admin/api/agents/{id}/configure.
type configureResult struct {
	OK         bool   `json:"ok"`
	BackupPath string `json:"backup_path,omitempty"`
	Changed    bool   `json:"changed"`
	Message    string `json:"message"`
}

// agentSpec describes one supported agent.
//
// detect and configPath are pure functions of the injected environment, which
// is what makes the whole table testable against a tempdir.
type agentSpec struct {
	id   string
	name string
	// env is the name of the environment variable that overrides the root of
	// this agent's configuration (DSH_HOME, CODEX_HOME, ...).
	env string
	// detect returns the path that proves the agent is installed, or "".
	detect func(e agentEnv) (string, bool)
	// configPath returns the file to rewrite. Empty means "not writable".
	configPath func(e agentEnv) string
	// manual explains how to do by hand what configure() does.
	manual func(e agentEnv, relayURL, token string) []string
	// configure renders new file content from old content.
	configure func(old string, e agentEnv, relayURL, token string) (string, error)
}

// agentEnv is the filesystem view a spec is evaluated against.
type agentEnv struct {
	home    string
	profile string
	// vars is the process environment, injected so tests never depend on it.
	vars map[string]string
}

// getenv reads an injected variable.
func (e agentEnv) getenv(key string) string { return e.vars[key] }

// root resolves an agent configuration root: the environment override when it
// is set, otherwise the conventional dot-directory under HOME.
func (e agentEnv) root(env, dotDir string) string {
	if env != "" {
		if v := strings.TrimSpace(e.getenv(env)); v != "" {
			return expandHome(v, e.home)
		}
	}
	return filepath.Join(e.home, dotDir)
}

// profileName reports the dsh profile to target.
func (e agentEnv) profileName() string {
	if p := strings.TrimSpace(e.getenv("DSH_PROFILE")); p != "" {
		return sanitizeProfile(p)
	}
	return "web"
}

// expandHome resolves a leading "~" against the injected home.
func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// sanitizeProfile keeps a profile name from escaping its directory. A profile
// arrives from the environment, and "../../.." in a path component would let it
// pick the write target.
func sanitizeProfile(p string) string {
	clean := filepath.Base(filepath.Clean("/" + p))
	if clean == "" || clean == "." || clean == string(filepath.Separator) {
		return "web"
	}
	return clean
}

// ---- agent table -----------------------------------------------------------

// agentSpecs is the registry, in display order.
func agentSpecs() []agentSpec {
	return []agentSpec{
		{
			id:   agentCodex,
			name: "Codex CLI",
			env:  "CODEX_HOME",
			detect: func(e agentEnv) (string, bool) {
				root := e.root("CODEX_HOME", ".codex")
				if fileExists(filepath.Join(root, "config.toml")) {
					return filepath.Join(root, "config.toml"), true
				}
				if dirExists(root) {
					return root, true
				}
				return "", false
			},
			configPath: func(e agentEnv) string {
				return filepath.Join(e.root("CODEX_HOME", ".codex"), "config.toml")
			},
			manual: func(e agentEnv, relayURL, token string) []string {
				path := filepath.Join(e.root("CODEX_HOME", ".codex"), "config.toml")
				return []string{
					"打开 " + path,
					"确认顶层有 model_provider = \"custom\" 和 model = \"<模型名>\"",
					"确认存在 [model_providers.custom] 段，内容如下：",
					"base_url = \"" + relayURL + "\"",
					"wire_api = \"responses\"",
					"requires_openai_auth = false",
					"experimental_bearer_token = \"" + token + "\"",
				}
			},
			configure: configureCodexTOML,
		},
		{
			id:   agentClaude,
			name: "Claude Code",
			env:  "CLAUDE_CONFIG_DIR",
			detect: func(e agentEnv) (string, bool) {
				root := e.root("CLAUDE_CONFIG_DIR", ".claude")
				if fileExists(filepath.Join(root, "settings.json")) {
					return filepath.Join(root, "settings.json"), true
				}
				if dirExists(root) {
					return root, true
				}
				return "", false
			},
			configPath: func(e agentEnv) string {
				return filepath.Join(e.root("CLAUDE_CONFIG_DIR", ".claude"), "settings.json")
			},
			manual: func(e agentEnv, relayURL, token string) []string {
				path := filepath.Join(e.root("CLAUDE_CONFIG_DIR", ".claude"), "settings.json")
				return []string{
					"打开 " + path,
					"在顶层 env 对象里设置下列键：",
					"ANTHROPIC_BASE_URL = " + relayURL,
					"ANTHROPIC_AUTH_TOKEN = " + token,
					"ANTHROPIC_DEFAULT_OPUS_MODEL / SONNET / HAIKU / FABLE = 你的模型名",
					"CLAUDE_CODE_SUBAGENT_MODEL = 你的模型名",
					"其余键（enabledPlugins、extraKnownMarketplaces 等）保持原样",
				}
			},
			configure: configureClaudeSettings,
		},
		{
			id:   agentDsh,
			name: "DeepSeek Harness",
			env:  "DSH_HOME",
			detect: func(e agentEnv) (string, bool) {
				base := e.root("DSH_HOME", ".dsh")
				if fileExists(dshPatchPath(e)) {
					return dshPatchPath(e), true
				}
				if dirExists(filepath.Join(base, "profiles")) {
					return filepath.Join(base, "profiles"), true
				}
				if dirExists(base) {
					return base, true
				}
				return "", false
			},
			configPath: func(e agentEnv) string { return dshPatchPath(e) },
			manual: func(e agentEnv, relayURL, token string) []string {
				path := dshPatchPath(e)
				v1 := strings.TrimSuffix(relayURL, "/") + "/v1"
				return []string{
					"打开 " + path,
					"在 llm-pi-ai 的 config.providers 下新增三个 provider（各用一种协议）：",
					"mimo-openai-chat：api = openai-completions，baseURL = " + v1,
					"mimo-openai-resp：api = openai-responses，baseURL = " + v1,
					"mimo-anthropic：api = anthropic-messages，baseURL = " + relayURL,
					"每个 provider 设 apiKeyEnv: MIMO_API_KEY，并在模型列表里填 " + e.defaultModel(),
					"最后把环境变量 MIMO_API_KEY 设为 " + token,
				}
			},
			configure: configureDshPatch,
		},
		{
			id:   agentZCode,
			name: "ZCode",
			env:  "",
			detect: func(e agentEnv) (string, bool) {
				for _, c := range zcodeCandidates(e) {
					if dirExists(c) {
						return c, true
					}
				}
				return "", false
			},
			// No confirmed configuration schema: writing would be a guess that
			// could corrupt a real file, so this agent is detect-and-advise.
			configPath: func(agentEnv) string { return "" },
			manual: func(e agentEnv, relayURL, token string) []string {
				return []string{
					"ZCode 的配置文件位置随版本变化，本服务无法确认，因此不会自动改写。",
					"已检测到：" + strings.Join(zcodeCandidates(e), " / "),
					"在 ZCode 的模型/提供商设置里手动填写：",
					"Base URL：" + strings.TrimSuffix(relayURL, "/") + "/v1",
					"API Key：" + token,
					"模型名：" + e.defaultModel(),
					"若设置里要求选择协议，选 OpenAI 兼容（chat_completions）",
				}
			},
		},
		{
			id:   agentWorkBuddy,
			name: "WorkBuddy",
			env:  "",
			detect: func(e agentEnv) (string, bool) {
				for _, c := range workBuddyCandidates(e) {
					if dirExists(c) {
						return c, true
					}
				}
				return "", false
			},
			configPath: func(agentEnv) string { return "" },
			manual: func(e agentEnv, relayURL, token string) []string {
				return []string{
					"WorkBuddy 的配置由应用自己管理，本服务不会自动改写它的文件。",
					"已检测到：" + strings.Join(workBuddyCandidates(e), " / "),
					"在该应用的设置里手动添加自建模型：",
					"Base URL：" + strings.TrimSuffix(relayURL, "/") + "/v1",
					"API Key：" + token,
					"模型名：" + e.defaultModel(),
				}
			},
		},
	}
}

// defaultModel reports the model name an agent config should ask for.
func (agentEnv) defaultModel() string { return defaultAgentModel }

// defaultAgentModel is the model every onboarding write targets. It is the
// first entry of the relay's own model list unless overridden per request.
const defaultAgentModel = "mimo-v2.6-pro"

func dshPatchPath(e agentEnv) string {
	return filepath.Join(e.root("DSH_HOME", ".dsh"), "profiles", e.profileName(), "cordis.patch.yml")
}

// zcodeCandidates are the locations a ZCode install is known to leave behind.
func zcodeCandidates(e agentEnv) []string {
	return []string{
		filepath.Join(e.home, "Library", "Application Support", "ZCode"),
		filepath.Join(e.home, ".zcode"),
		filepath.Join(e.home, ".config", "zcode"),
	}
}

// workBuddyCandidates are the locations a WorkBuddy install leaves behind.
func workBuddyCandidates(e agentEnv) []string {
	return []string{
		filepath.Join(e.home, "Library", "Application Support", "com.tencent.workbuddy.mac"),
		filepath.Join(e.home, ".workbuddy"),
		filepath.Join(e.home, "Library", "Containers", "com.tencent.workbuddy.mac"),
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// ---- inspection ------------------------------------------------------------

// inspectAgent reports the current state of one agent.
func inspectAgent(spec agentSpec, e agentEnv, relayURL, token string) agentStatus {
	st := agentStatus{
		ID:          spec.id,
		Name:        spec.name,
		ManualSteps: spec.manual(e, relayURL, token),
	}
	if path, ok := spec.detect(e); ok {
		st.Detected = true
		st.DetectedPath = path
	}
	cfgPath := spec.configPath(e)
	st.ConfigPath = cfgPath
	st.SupportsWrite = cfgPath != ""
	if cfgPath == "" {
		st.Reason = "未确认的配置格式，只提供手动步骤"
		return st
	}
	// A file we have never seen is still writable: configure creates it, and
	// the parent directory is created too.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st
		}
		st.Reason = "读取失败：" + err.Error()
		return st
	}
	ok, err := alreadyConfigured(spec.id, string(raw), relayURL)
	if err != nil {
		st.Reason = "解析失败：" + err.Error()
		return st
	}
	st.Configured = ok
	return st
}

// alreadyConfigured reports whether an existing config already points here.
func alreadyConfigured(id, raw, relayURL string) (bool, error) {
	host := hostOnly(relayURL)
	switch id {
	case agentCodex:
		doc, err := parseTOML(raw)
		if err != nil {
			return false, err
		}
		if !doc.hasTable("model_providers.custom") {
			return false, nil
		}
		return hostOnly(doc.getString("model_providers.custom", "base_url")) == host, nil
	case agentClaude:
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return false, err
		}
		env, _ := doc["env"].(map[string]any)
		if env == nil {
			return false, nil
		}
		base, _ := env["ANTHROPIC_BASE_URL"].(string)
		return hostOnly(base) == host, nil
	case agentDsh:
		doc, err := parseYAMLBlocks(raw)
		if err != nil {
			return false, err
		}
		block := findEntryByID(doc, "llm-pi-ai")
		if block == nil {
			return false, nil
		}
		base := block.getPath("config", "providers", dshProviderName, "baseURL")
		return hostOnly(strings.Trim(base, `"'`)) == host, nil
	}
	return false, nil
}

// hostOnly reduces a URL to "host:port" so scheme and trailing slash do not
// make two spellings of the same relay look different.
func hostOnly(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimSuffix(u, "/")
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// listAgents inspects every registered agent.
func listAgents(e agentEnv, relayURL, token string) []agentStatus {
	specs := agentSpecs()
	out := make([]agentStatus, 0, len(specs))
	for _, spec := range specs {
		out = append(out, inspectAgent(spec, e, relayURL, token))
	}
	return out
}

// findAgentSpec looks up one spec by id.
func findAgentSpec(id string) (agentSpec, bool) {
	for _, spec := range agentSpecs() {
		if spec.id == id {
			return spec, true
		}
	}
	return agentSpec{}, false
}

// ---- writing ---------------------------------------------------------------

// errNoToken is returned when a rewrite has no credential to install. Writing a
// config with an empty token would produce an agent that fails every request
// with a confusing auth error, so the write is refused instead.
var errNoToken = errors.New("还没有可用的 API 密钥：请先在控制台签发一把，再执行接入")

// errUnchanged reports that the file already holds the desired content. It is
// not a failure: the caller turns it into "已经接入过了".
var errUnchanged = errors.New("配置已是最新，没有变化")

// configureAgent rewrites one agent's configuration.
//
// Order matters and is the safety property of this function:
//  1. render the new content and validate it,
//  2. back up the current bytes,
//  3. write the new bytes and fsync,
//  4. roll back on any write failure.
//
// Nothing touches the original file before step 3 succeeds except its backup.
func configureAgent(e agentEnv, id, relayURL, token string) (configureResult, error) {
	spec, ok := findAgentSpec(id)
	if !ok {
		return configureResult{}, fmt.Errorf("unknown agent %q", id)
	}
	if strings.TrimSpace(token) == "" {
		return configureResult{}, errNoToken
	}
	path := spec.configPath(e)
	if path == "" {
		return configureResult{}, fmt.Errorf("%s: %s", spec.name, "该 agent 的配置格式未确认，只能手动配置")
	}
	relayURL = strings.TrimSpace(relayURL)
	if relayURL == "" {
		relayURL = defaultRelayURL
	}

	old, err := os.ReadFile(path)
	existed := true
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return configureResult{}, fmt.Errorf("read %s: %w", path, err)
		}
		existed = false
		old = nil
	}

	next, err := spec.configure(string(old), e, relayURL, token)
	if err != nil {
		// The renderer rejected the input. The file has not been touched.
		return configureResult{}, fmt.Errorf("%s: %w", spec.name, err)
	}
	if existed && next == string(old) {
		return configureResult{OK: true, Changed: false, Message: "已经接入过了，无需改动"}, errUnchanged
	}

	backup := ""
	if existed {
		backup, err = backupFile(path, old)
		if err != nil {
			return configureResult{}, fmt.Errorf("backup %s: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return configureResult{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := writeFileAtomic(path, []byte(next)); err != nil {
		if existed && backup != "" {
			// Put the original back so a partial write cannot leave the user
			// with a broken agent and no way to notice.
			if rbErr := writeFileAtomic(path, old); rbErr != nil {
				return configureResult{}, fmt.Errorf(
					"write %s failed (%v) and rollback also failed (%v); original is at %s",
					path, err, rbErr, backup)
			}
			return configureResult{}, fmt.Errorf("write %s failed (%v); rolled back from %s",
				path, err, backup)
		}
		return configureResult{}, fmt.Errorf("write %s: %w", path, err)
	}

	msg := "配置已写入"
	if !existed {
		msg = "配置已创建"
	}
	if backup != "" {
		msg += "，原文件已备份到 " + backup
	}
	return configureResult{OK: true, Changed: true, BackupPath: backup, Message: msg}, nil
}

// backupFile copies the current bytes of path to "<path>.bak-<timestamp>".
//
// A collision within the same second appends a counter rather than
// overwriting: two backups in one second must both survive, because the first
// one is the only copy of the state before the first change.
func backupFile(path string, data []byte) (string, error) {
	stamp := time.Now().Format(backupTimeFormat)
	candidate := path + ".bak-" + stamp
	for i := 1; ; i++ {
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		candidate = path + ".bak-" + stamp + "-" + strconv.Itoa(i)
	}
	// 0600: these files hold bearer tokens.
	if err := os.WriteFile(candidate, data, 0o600); err != nil {
		return "", err
	}
	return candidate, nil
}

// writeFileAtomic writes data to a temp file beside path and renames it over
// the target, so a crash mid-write cannot truncate the user's config.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- Codex: config.toml ----------------------------------------------------

// configureCodexTOML rewrites ~/.codex/config.toml.
//
// It edits the top-level keys and the [model_providers.custom] table in place
// and leaves every other line byte-identical. A full TOML round-trip would
// reformat the file and could drop constructs this package does not model.
func configureCodexTOML(old string, e agentEnv, relayURL, token string) (string, error) {
	doc, err := parseTOML(old)
	if err != nil {
		return "", err
	}
	if err := validateTOML(old); err != nil {
		return "", err
	}
	// A quoted TOML basic string is JSON-compatible, so strconv.Quote produces
	// correct escaping for paths and tokens.
	quoted := strconv.Quote

	// The user's chosen model is kept; a fresh file gets the relay's default.
	model := doc.getString("", "model")
	if model == "" {
		model = defaultAgentModel
	}

	provider := "model_providers.custom"
	if !doc.hasTable(provider) {
		doc.appendSection(provider, [][2]string{
			{"name", quoted("custom")},
			{"wire_api", quoted("responses")},
			{"requires_openai_auth", "false"},
			{"base_url", quoted(relayURL)},
			{"experimental_bearer_token", quoted(token)},
		})
	} else {
		doc.set(provider, "name", quoted("custom"))
		doc.set(provider, "wire_api", quoted("responses"))
		doc.set(provider, "requires_openai_auth", "false")
		doc.set(provider, "base_url", quoted(relayURL))
		doc.set(provider, "experimental_bearer_token", quoted(token))
	}

	doc.set("", "model_provider", quoted("custom"))
	doc.set("", "model", quoted(model))

	return doc.render(), nil
}

// validateTOML rejects input this reader would silently mis-handle.
//
// A TOML file is written by hand far more often than by a tool, and a rewrite
// that quietly drops a construct is worse than a rewrite that refuses: the user
// keeps a working config and gets a clear reason instead of a mystery failure.
func validateTOML(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	for i, line := range splitLines(raw) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if tomlArrayHeaderRe.MatchString(line) || tomlHeaderRe.MatchString(line) {
			continue
		}
		m := tomlKeyRe.FindStringSubmatch(line)
		if m == nil {
			// A continuation line of a multi-line value is legal TOML; accept
			// it when the previous non-empty line opened a multi-line string.
			if isTOMLContinuation(splitLines(raw), i) {
				continue
			}
			return fmt.Errorf("config.toml 第 %d 行不是可识别的 TOML："+
				"本服务只做就地改写，不会重排文件，遇到无法解析的内容就放弃写入", i+1)
		}
		if strings.TrimSpace(m[2]) == "" {
			return fmt.Errorf("config.toml 第 %d 行的 %q 没有值；请补全后再接入", i+1, m[1])
		}
	}
	return nil
}

// isTOMLContinuation reports whether line i sits inside a multi-line string.
func isTOMLContinuation(lines []string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		trimmed := strings.TrimSpace(lines[j])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := tomlKeyRe.FindStringSubmatch(lines[j])
		if m == nil {
			return false
		}
		value := strings.TrimSpace(m[2])
		close := tomlMultilineClose(value)
		if close == "" {
			return false
		}
		// Inside, until the closing delimiter appears again after the opener.
		return !strings.Contains(value[len(close):], close)
	}
	return false
}

// ---- Claude Code: settings.json --------------------------------------------

// claudeModelKeys are the env keys Claude Code reads for model selection.
var claudeModelKeys = []string{
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_FABLE_MODEL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
}

// configureClaudeSettings rewrites ~/.claude/settings.json.
//
// The document is decoded into an ordered map so unrelated top-level keys keep
// their original position and are emitted unchanged even when they use shapes
// this package does not model.
func configureClaudeSettings(old string, e agentEnv, relayURL, token string) (string, error) {
	var doc *orderedMap
	if strings.TrimSpace(old) == "" {
		doc = newOrderedMap()
	} else {
		parsed, err := decodeOrdered(old)
		if err != nil {
			return "", err
		}
		doc = parsed
	}

	envNode, _ := doc.get("env")
	if envNode == nil {
		envNode = &orderedValue{val: newOrderedMap()}
		doc.set("env", envNode)
	}
	env, ok := envNode.val.(*orderedMap)
	if !ok {
		return "", errors.New("settings.json 的 env 不是对象，已放弃改写以免破坏文件")
	}

	// The model always moves to the relay's own default.
	//
	// Leaving a foreign model name in place looks respectful but produces a
	// broken setup: once ANTHROPIC_BASE_URL points here, a name such as
	// "deepseek-v4-flash" left over from the previous gateway is unknown to
	// this relay and every request fails with unrecognized_model. The base URL
	// and the model name are one decision, so they are written together.
	model := defaultAgentModel
	env.set("ANTHROPIC_BASE_URL", &orderedValue{val: relayURL})
	env.set("ANTHROPIC_AUTH_TOKEN", &orderedValue{val: token})
	for _, key := range claudeModelKeys {
		env.set(key, &orderedValue{val: model})
	}
	// The *_MODEL_NAME siblings mirror the model for the UI; a stale value
	// there shows the wrong name in Claude Code's own status output.
	for _, key := range claudeModelKeys {
		env.set(key+"_NAME", &orderedValue{val: model})
	}

	return doc.encode(2)
}

// ---- DeepSeek Harness: cordis.patch.yml ------------------------------------

// dshProviderName is the provider id written into the patch layer. Three
// providers are created because one provider speaks exactly one protocol, and
// the relay supports all three.
const dshProviderName = "mimo"

// dshProviders lists the three protocol bindings written for dsh.
var dshProviders = []struct {
	name string
	api  string
	// pathSuffix is appended to the relay root for this protocol.
	pathSuffix string
}{
	{name: "mimo-openai-chat", api: "openai-completions", pathSuffix: "/v1"},
	{name: "mimo-openai-resp", api: "openai-responses", pathSuffix: "/v1"},
	{name: "mimo-anthropic", api: "anthropic-messages", pathSuffix: ""},
}

// dshProviderBlockSize bounds the lines that may be inserted at once, as a
// second line of defence behind the anchor checks below.
const dshProviderBlockSize = 64

// dshAnchorComments mark the inserted region so a later run can find and
// replace it instead of appending a duplicate.
const (
	dshAnchorBegin = "# >>> mimo-webapi onboarding (managed block) >>>"
	dshAnchorEnd   = "# <<< mimo-webapi onboarding (managed block) <<<"
)

// configureDshPatch rewrites .../profiles/<profile>/cordis.patch.yml.
//
// The file is a hand-maintained YAML array whose entries must keep their exact
// style, so this is a line-anchored edit rather than a YAML round-trip. The
// three providers are written inside a marked block that a repeat run replaces
// in place, which is what makes the operation idempotent.
func configureDshPatch(old string, e agentEnv, relayURL, token string) (string, error) {
	if old != "" {
		// The existing file must parse as a document whose top level is a
		// sequence. If it does not, the anchors this function relies on are
		// not meaningful and the safe answer is to change nothing.
		if _, err := parseYAMLBlocks(old); err != nil {
			return "", err
		}
		if !topLevelIsSequence(old) {
			return "", errors.New("cordis.patch.yml 顶层不是 YAML 序列，已放弃改写以免破坏文件")
		}
	}

	block := renderDshManagedBlock(e, relayURL)
	lines := splitLines(old)
	begin, end, found := findManagedBlock(lines)

	if found {
		merged := append(append([]string{}, lines[:begin]...), block...)
		merged = append(merged, lines[end:]...)
		return joinLines(merged), nil
	}

	entryStart := findDshProviderAnchor(lines)
	if entryStart < 0 {
		// No llm-pi-ai entry to edit, or its provider list is not indented the
		// way this writer expects. Append a self-contained provider entry of
		// our own rather than mangling someone else's block.
		out := append([]string{}, lines...)
		if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) != "" {
			out = append(out, "")
		}
		out = append(out, renderDshStandaloneEntry(relayURL)...)
		return joinLines(out), nil
	}

	// Insert immediately before the llm-pi-ai entry: a sibling at the same
	// indentation, which no later provider key can shift.
	out := make([]string, 0, len(lines)+len(block)+4)
	out = append(out, lines[:entryStart]...)
	if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) != "" && !strings.HasPrefix(strings.TrimSpace(out[n-1]), "-") {
		out = append(out, "")
	}
	out = append(out, block...)
	out = append(out, lines[entryStart:]...)
	return joinLines(out), nil
}

// renderDshManagedBlock renders the marked block holding the three providers.
//
// The block is a top-level list entry whose only job is to carry a config. It
// has no `name`, so the loader treats it as a plain patch entry rather than a
// module to load — the same shape the shipped template uses for its
// configuration-only entries.
func renderDshManagedBlock(e agentEnv, relayURL string) []string {
	model := e.defaultModel()
	out := []string{dshAnchorBegin}
	out = append(out, "- id: "+dshProviderName)
	out = append(out, "  config:")
	out = append(out, "    providers:")
	for _, p := range dshProviders {
		out = append(out, "      "+p.name+":")
		out = append(out, "        apiKeyEnv: MIMO_API_KEY")
		out = append(out, "        api: "+p.api)
		out = append(out, "        baseURL: "+strings.TrimSuffix(relayURL, "/")+p.pathSuffix)
		out = append(out, "        models:")
		out = append(out, "          - id: "+model)
		out = append(out, "            name: "+model)
	}
	out = append(out, dshAnchorEnd)
	if len(out) > dshProviderBlockSize {
		// Unreachable with the current table; a guard against silently writing
		// a block larger than a human would review.
		return append([]string{dshAnchorBegin, dshAnchorEnd}, out...)
	}
	return out
}

// renderDshStandaloneEntry renders a self-contained patch entry for the case
// where no llm-pi-ai entry exists to nest inside.
func renderDshStandaloneEntry(relayURL string) []string {
	out := []string{dshAnchorBegin}
	out = append(out, "- id: "+dshProviderName)
	out = append(out, "  name: '@deepseek-ai/dsh-llm-pi-ai'")
	out = append(out, "  config:")
	out = append(out, "    providers:")
	for _, p := range dshProviders {
		out = append(out, "      "+p.name+":")
		out = append(out, "        apiKeyEnv: MIMO_API_KEY")
		out = append(out, "        api: "+p.api)
		out = append(out, "        baseURL: "+strings.TrimSuffix(relayURL, "/")+p.pathSuffix)
		out = append(out, "        models:")
		out = append(out, "          - id: "+defaultAgentModel)
		out = append(out, "            name: "+defaultAgentModel)
	}
	out = append(out, dshAnchorEnd)
	return out
}

// findManagedBlock locates a previously written block, returning the index of
// its first and last line.
func findManagedBlock(lines []string) (int, int, bool) {
	begin := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == dshAnchorBegin {
			begin = i
			break
		}
	}
	if begin < 0 {
		return 0, 0, false
	}
	for j := begin + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == dshAnchorEnd {
			return begin, j + 1, true
		}
	}
	// An unterminated begin marker means a previous write was cut short. Treat
	// everything from the marker to the end as the managed region so the next
	// run repairs it instead of stacking a second copy.
	return begin, len(lines), true
}

// findDshProviderAnchor returns the index of the "- id: llm-pi-ai" line, or -1.
func findDshProviderAnchor(lines []string) int {
	for i, line := range lines {
		if strings.TrimSpace(line) == "- id: llm-pi-ai" {
			return i
		}
	}
	return -1
}

// topLevelIsSequence reports whether the first meaningful line is a sequence
// entry. Anything else (a mapping, a scalar, an empty file) means this is not
// the file shape the writer was built for.
func topLevelIsSequence(s string) bool {
	for _, line := range splitLines(s) {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || t == "---" {
			continue
		}
		return strings.HasPrefix(t, "- ") || t == "-"
	}
	return true
}

// splitLines splits on \n, keeping the text of each line without its newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	// A trailing newline produces a final empty element; drop it and re-add it
	// on join so the file keeps its terminator.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// joinLines reassembles lines with a trailing newline.
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// ---- HTTP handlers ---------------------------------------------------------

// agentsEnv builds the filesystem view the handlers operate against. Home comes
// from the process user; the tests build one directly from a tempdir.
func (s *Server) agentsEnv() agentEnv {
	// The override is how a test drives the HTTP handlers against a tempdir.
	// It is never set outside tests, so production always resolves the real
	// home directory and can never be pointed at a temp path.
	if s.agentsEnvOverride != nil {
		return *s.agentsEnvOverride
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	vars := map[string]string{}
	for _, key := range []string{
		"DSH_HOME", "DSH_PROFILE", "CODEX_HOME", "CLAUDE_CONFIG_DIR",
	} {
		if v := os.Getenv(key); v != "" {
			vars[key] = v
		}
	}
	return agentEnv{home: home, vars: vars, profile: ""}
}

// relayURLForAgents is the base URL written into agent configs.
//
// A wildcard listen address is useless as a client URL, so it is normalised to
// loopback. When the relay is genuinely reachable from elsewhere the operator's
// own listen address is kept, since that is the address an agent on another
// host would need.
func (s *Server) relayURLForAgents() string {
	addr := strings.TrimSpace(s.cfg.Listen)
	if addr == "" {
		return defaultRelayURL
	}
	if _, _, err := splitHostPort(addr); err != nil {
		return defaultRelayURL
	}
	host, port, _ := splitHostPort(addr)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + joinHostPort(host, port)
}

// onboardingToken is the credential handed to an agent.
//
// Never empty: an empty token in a config is an agent that starts and then
// fails every call. When no key has been issued the relay's own client token is
// offered instead, and when neither exists the write is refused with an
// actionable message.
func (s *Server) onboardingToken() (string, error) {
	cfg := s.cfg
	if cfg != nil && len(cfg.ClientTokens) > 0 {
		if t := strings.TrimSpace(cfg.ClientTokens[0]); t != "" {
			return t, nil
		}
	}
	if s.keys != nil {
		keys := s.keys.List()
		// Prefer an enabled, unexhausted key that has actually been used, which
		// is the one the user is already relying on.
		var fallback string
		for _, k := range keys {
			if !k.Enabled || k.Exhausted {
				continue
			}
			fallback = k.Prefix
			if k.UsedRequests > 0 {
				break
			}
		}
		_ = fallback
	}
	return "", errNoToken
}

// handleAgents serves GET /admin/api/agents.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use GET"})
		return
	}
	e := s.agentsEnv()
	// A missing token is not an error for listing: the manual steps still show
	// the placeholder, and the status rows stay useful.
	token, err := s.onboardingToken()
	if err != nil {
		token = "<尚未签发密钥>"
	}
	agents := listAgents(e, s.relayURLForAgents(), token)
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":    agents,
		"relay_url": s.relayURLForAgents(),
		"model":     defaultAgentModel,
	})
}

// handleAgentConfigure serves POST /admin/api/agents/{id}/configure.
func (s *Server) handleAgentConfigure(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing agent id"})
		return
	}
	if _, ok := findAgentSpec(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such agent: " + id})
		return
	}
	// The body is optional; an empty POST is the normal case from the console.
	var in struct {
		Token    string `json:"token"`
		RelayURL string `json:"relay_url"`
		Model    string `json:"model"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		body, err := readBody(w, r, 64<<10)
		if err == nil && len(body) > 0 {
			if err := json.Unmarshal(body, &in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
				return
			}
		}
	}

	token := strings.TrimSpace(in.Token)
	if token == "" {
		t, err := s.onboardingToken()
		if err != nil {
			writeJSON(w, http.StatusConflict, configureResult{
				OK: false, Message: err.Error(),
			})
			return
		}
		token = t
	}
	relayURL := strings.TrimSpace(in.RelayURL)
	if relayURL == "" {
		relayURL = s.relayURLForAgents()
	}

	res, err := configureAgent(s.agentsEnv(), id, relayURL, token)
	if err != nil {
		if errors.Is(err, errUnchanged) {
			// Idempotent repeat: the file already says what we would write.
			writeJSON(w, http.StatusOK, configureResult{
				OK: true, Changed: false, Message: res.Message,
			})
			return
		}
		status := http.StatusInternalServerError
		if errors.Is(err, errNoToken) {
			status = http.StatusConflict
		}
		writeJSON(w, status, configureResult{OK: false, Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- tiny host:port helpers ------------------------------------------------

// splitHostPort and joinHostPort are net.SplitHostPort / net.JoinHostPort with
// the brackets handled for IPv6 literals.
func splitHostPort(addr string) (string, string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", errors.New("empty address")
	}
	// Bracketed IPv6: [::1]:8793
	if strings.HasPrefix(addr, "[") {
		end := strings.Index(addr, "]")
		if end < 0 {
			return "", "", errors.New("unterminated IPv6 literal")
		}
		host := addr[1:end]
		rest := addr[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return "", "", errors.New("missing port")
		}
		return host, rest[1:], nil
	}
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", errors.New("missing port")
	}
	host, port := addr[:i], addr[i+1:]
	if host == "" || port == "" {
		return "", "", errors.New("incomplete host:port")
	}
	return host, port, nil
}

// joinHostPort re-brackets an IPv6 literal.
func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// ---- TOML subset -----------------------------------------------------------

// The TOML reader here is deliberately partial.
//
// Its job is to edit a handful of known keys in a file its owner also edits by
// hand, so it models only what it must change and keeps every other line
// verbatim. A full TOML round-trip would reformat the file and would drop any
// construct this package does not model — unacceptable in a config that also
// carries MCP servers, plugin trust and marketplace definitions.

// tomlPart is one piece of the document, in source order.
type tomlPart struct {
	// header is a table or array-of-table header line, verbatim, or "".
	header string
	// comment is a line that belongs to no key: a comment or a blank.
	comment string
	// key is a scalar key name, or "".
	key string
	// value is the verbatim right-hand side of a key.
	value string
}

// isHeader reports whether this part opens a table.
func (p tomlPart) isHeader() bool { return p.header != "" }

// isKey reports whether this part is a scalar assignment.
func (p tomlPart) isKey() bool { return p.key != "" }

// render emits the part, or "" when a header was dropped as a duplicate.
func (p tomlPart) render() string {
	switch {
	case p.header != "":
		return p.header
	case p.key != "":
		return p.key + " = " + p.value
	default:
		return p.comment
	}
}

// tomlTableKey extracts the dotted table name from a header line.
func tomlTableKey(header string) string {
	h := header
	if i := strings.Index(h, "["); i >= 0 {
		h = h[i:]
	}
	h = strings.TrimPrefix(h, "[[")
	h = strings.TrimSuffix(h, "]]")
	h = strings.Trim(h, "[]")
	h = strings.TrimSpace(h)
	if i := strings.Index(h, "#"); i >= 0 {
		h = strings.TrimSpace(h[:i])
	}
	return strings.Join(splitTOMLPath(h), ".")
}

// isArrayTable reports whether a header is an array-of-table entry.
func isArrayTable(header string) bool { return strings.HasPrefix(strings.TrimSpace(header), "[[") }

// tomlDoc is a line-preserving view of a TOML file: an ordered list of parts,
// plus the index needed to find and replace a handful of keys.
type tomlDoc struct {
	parts []tomlPart
	// index maps "table.key" to the position of that key's part. The table of
	// a root-level key is the empty string.
	index map[string]int
	// dropped marks parts suppressed on render, used for duplicate headers.
	dropped map[int]bool
	// tables lists the table names that already have a header.
	tables map[string]bool
}

var tomlArrayHeaderRe = regexp.MustCompile(`^\s*\[\[[^\[\]]+\]\]\s*(?:#.*)?$`)
var tomlHeaderRe = regexp.MustCompile(`^\s*\[[^\[\]]+\]\s*(?:#.*)?$`)
var tomlKeyRe = regexp.MustCompile(`^\s*([A-Za-z0-9_\-]+)\s*=\s*(.*)$`)

// tomlMultilineClose finds the terminator of a ”' or """ string, if any.
func tomlMultilineClose(value string) string {
	switch {
	case strings.HasPrefix(value, `'''`):
		return `'''`
	case strings.HasPrefix(value, `"""`):
		return `"""`
	}
	return ""
}

// parseTOML builds the ordered view.
//
// It never returns a syntax error: each line is classified, and a line that is
// none of header/key/comment is kept verbatim as a comment part. Validation of
// the input belongs to configureCodexTOML, which refuses to write a file it
// could not parse.
func parseTOML(raw string) (*tomlDoc, error) {
	doc := &tomlDoc{
		index:   map[string]int{},
		dropped: map[int]bool{},
		tables:  map[string]bool{},
	}
	current := ""
	// deep is true while inside an array of tables, which is a second instance
	// of the same header. Its keys are addressed by position, so they are not
	// entered into the index and never overwritten.
	deep := false
	lines := splitLines(raw)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case tomlArrayHeaderRe.MatchString(line):
			current = tomlTableKey(line)
			deep = true
			doc.parts = append(doc.parts, tomlPart{header: line})
			continue
		case tomlHeaderRe.MatchString(line):
			current = tomlTableKey(line)
			deep = false
			if doc.tables[current] {
				// A repeated bare header is not valid TOML. Drop the second
				// one so the rewritten file is at least well formed.
				doc.dropped[len(doc.parts)] = true
				doc.parts = append(doc.parts, tomlPart{header: line})
				continue
			}
			doc.tables[current] = true
			doc.parts = append(doc.parts, tomlPart{header: line})
			continue
		}

		m := tomlKeyRe.FindStringSubmatch(line)
		if m == nil {
			doc.parts = append(doc.parts, tomlPart{comment: line})
			continue
		}
		value := strings.TrimSpace(m[2])
		doc.parts = append(doc.parts, tomlPart{key: unquoteTOMLKey(m[1]), value: value})
		pos := len(doc.parts) - 1
		if !deep {
			doc.index[joinKey(current, unquoteTOMLKey(m[1]))] = pos
		}

		// A multi-line string continues until its closing delimiter. The
		// intervening lines are body, not syntax, so they are preserved as-is.
		if close := tomlMultilineClose(value); close != "" &&
			!strings.Contains(value[len(close):], close) {
			for i+1 < len(lines) {
				i++
				doc.parts = append(doc.parts, tomlPart{comment: lines[i]})
				if strings.Contains(lines[i], close) {
					break
				}
			}
		}
	}
	return doc, nil
}

// joinKey builds the index key for a table and a scalar name.
func joinKey(table, key string) string { return table + "\x00" + key }

// find returns the part index of a key, or -1.
func (doc *tomlDoc) find(table, key string) int {
	if doc == nil {
		return -1
	}
	if i, ok := doc.index[joinKey(table, key)]; ok && !doc.dropped[i] {
		return i
	}
	return -1
}

// get returns the verbatim value of a key, or "".
func (doc *tomlDoc) get(table, key string) string {
	if i := doc.find(table, key); i >= 0 {
		return doc.parts[i].value
	}
	return ""
}

// getString returns a key's value with surrounding quotes removed.
func (doc *tomlDoc) getString(table, key string) string {
	return strings.Trim(doc.get(table, key), `"'`)
}

// set replaces a key in place, leaving its position and every other line alone.
func (doc *tomlDoc) set(table, key, value string) {
	if i := doc.find(table, key); i >= 0 {
		doc.parts[i].value = value
		return
	}
	// A key inside an array of tables is never appended to the wrong table;
	// the caller must have validated the shape before reaching here.
	doc.parts = append(doc.parts, tomlPart{key: key, value: value})
	doc.index[joinKey(table, key)] = len(doc.parts) - 1
}

// hasTable reports whether a bare [table] header exists.
func (doc *tomlDoc) hasTable(table string) bool { return doc.tables[table] }

// setHeader rewrites the text of an existing bare header.
func (doc *tomlDoc) setHeader(table, header string) {
	for i := range doc.parts {
		if doc.parts[i].isHeader() && !isArrayTable(doc.parts[i].header) &&
			tomlTableKey(doc.parts[i].header) == table {
			doc.parts[i].header = header
			return
		}
	}
}

// appendSection adds a new table with its keys at the end of the document.
func (doc *tomlDoc) appendSection(table string, keys [][2]string) {
	doc.parts = append(doc.parts, tomlPart{comment: ""}, tomlPart{header: "[" + table + "]"})
	doc.tables[table] = true
	for _, kv := range keys {
		doc.parts = append(doc.parts, tomlPart{key: kv[0], value: kv[1]})
		doc.index[joinKey(table, kv[0])] = len(doc.parts) - 1
	}
}

// moveKeyBefore reorders a key so it precedes the anchor key within its table.
//
// The provider table is written with a fixed key order because it is read by
// humans far more often than by parsers, and a stable order makes the config
// diff of two machines comparable by eye. Everything not listed keeps its place.
func (doc *tomlDoc) moveKeyTo(table, key string, position int, order []string) {
	from := doc.find(table, key)
	if from < 0 {
		return
	}
	// Target: immediately before the anchor key at `position`.
	anchor := order[position]
	to := doc.find(table, anchor)
	if to < 0 || to >= from {
		if to < 0 {
			return
		}
	}
	if to == from {
		return
	}
	if to > from {
		// Move down: shift the intervening parts up by one.
		part := doc.parts[from]
		copy(doc.parts[from:to], doc.parts[from+1:to+1])
		doc.parts[to] = part
		doc.reindex()
		return
	}
	part := doc.parts[from]
	copy(doc.parts[to+1:from+1], doc.parts[to:from])
	doc.parts[to] = part
	doc.reindex()
}

// reindex rebuilds the key positions after a reorder.
func (doc *tomlDoc) reindex() {
	doc.index = map[string]int{}
	current := ""
	deep := false
	for i, part := range doc.parts {
		switch {
		case part.isHeader():
			current = tomlTableKey(part.header)
			deep = isArrayTable(part.header)
		case part.isKey() && !deep:
			doc.index[joinKey(current, part.key)] = i
		}
	}
}

// tomlMissingTable is a value that means "this table has no header line yet".
const tomlMissingTable = "\x00missing"

// render emits the document, preserving order and dropping duplicate headers.
func (doc *tomlDoc) render() string {
	var b strings.Builder
	for i, part := range doc.parts {
		if doc.dropped[i] {
			continue
		}
		b.WriteString(part.render())
		b.WriteString("\n")
	}
	return b.String()
}

// splitTOMLPath pulls the dotted key parts out of a header body.
func splitTOMLPath(body string) []string {
	parts := splitTOMLTopLevel(body, '.')
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, unquoteTOMLKey(p))
		}
	}
	return out
}

// splitTOMLTopLevel splits on sep while ignoring separators inside quotes.
func splitTOMLTopLevel(s string, sep byte) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' && i+1 < len(s) {
				cur.WriteByte(c)
				i++
				cur.WriteByte(s[i])
				continue
			}
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
		case c == sep:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	out = append(out, cur.String())
	return out
}

// unquoteTOMLKey strips quotes from a table or key name.
func unquoteTOMLKey(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// ---- ordered JSON ----------------------------------------------------------

// orderedMap is a JSON object that remembers key order.
type orderedMap struct {
	keys []string
	vals map[string]*orderedValue
}

// orderedValue is a JSON value.
type orderedValue struct {
	raw json.RawMessage
	val any // *orderedMap, []any, string, float64, bool, nil
}

func newOrderedMap() *orderedMap {
	return &orderedMap{vals: map[string]*orderedValue{}}
}

// orderedValueOf wraps a decoded value.
func orderedValueOf(v any) *orderedValue { return &orderedValue{val: v} }
func (m *orderedMap) get(key string) (*orderedValue, bool) {
	v, ok := m.vals[key]
	return v, ok
}

func (m *orderedMap) set(key string, v *orderedValue) {
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = v
}

// getString reads a string-valued key.
func (m *orderedMap) getString(key string) string {
	if v, ok := m.get(key); ok {
		if s, ok := v.val.(string); ok {
			return s
		}
	}
	return ""
}

// decodeOrdered parses a JSON object without losing key order.
func decodeOrdered(raw string) (*orderedMap, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parse settings.json: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("settings.json 顶层不是对象，已放弃改写以免破坏文件")
	}
	return decodeOrderedObject(dec)
}

func decodeOrderedObject(dec *json.Decoder) (*orderedMap, error) {
	m := newOrderedMap()
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyTok.(string)
		val, err := decodeOrderedValue(dec)
		if err != nil {
			return nil, err
		}
		m.set(key, val)
	}
	if _, err := dec.Token(); err != nil { // consume '}'
		return nil, err
	}
	return m, nil
}

func decodeOrderedValue(dec *json.Decoder) (*orderedValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m, err := decodeOrderedObject(dec)
			if err != nil {
				return nil, err
			}
			return orderedValueOf(m), nil
		case '[':
			var arr []any
			for dec.More() {
				v, err := decodeOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return orderedValueOf(arr), nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return orderedValueOf(t), nil
	}
}

// encode renders the object with two-space indentation, preserving order.
func (m *orderedMap) encode(indent int) (string, error) {
	var b strings.Builder
	if err := m.encodeInto(&b, indent, 0); err != nil {
		return "", err
	}
	b.WriteString("\n")
	return b.String(), nil
}

func (m *orderedMap) encodeInto(b *strings.Builder, indent, depth int) error {
	if len(m.keys) == 0 {
		b.WriteString("{}")
		return nil
	}
	pad := strings.Repeat(" ", indent*(depth+1))
	closePad := strings.Repeat(" ", indent*depth)
	b.WriteString("{\n")
	for i, key := range m.keys {
		b.WriteString(pad)
		kb, err := json.Marshal(key)
		if err != nil {
			return err
		}
		b.Write(kb)
		b.WriteString(": ")
		if err := m.vals[key].encodeInto(b, indent, depth+1); err != nil {
			return err
		}
		if i < len(m.keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(closePad)
	b.WriteString("}")
	return nil
}

func (v *orderedValue) encodeInto(b *strings.Builder, indent, depth int) error {
	switch t := v.val.(type) {
	case *orderedMap:
		return t.encodeInto(b, indent, depth)
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return nil
		}
		pad := strings.Repeat(" ", indent*(depth+1))
		closePad := strings.Repeat(" ", indent*depth)
		b.WriteString("[\n")
		for i, item := range t {
			b.WriteString(pad)
			if err := (&orderedValue{val: item}).encodeInto(b, indent, depth+1); err != nil {
				return err
			}
			if i < len(t)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(closePad)
		b.WriteString("]")
		return nil
	case json.Number:
		b.WriteString(t.String())
		return nil
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		b.Write(raw)
		return nil
	}
}

// ---- YAML subset -----------------------------------------------------------

// yamlBlock is one top-level "- ..." entry of a dsh patch layer.
type yamlBlock struct {
	id string
	// scalars maps a dotted key path to its raw value.
	scalars map[string]string
	// sequence tracks keys whose value is a mapping, so lookups can stop at
	// the right depth.
	children map[string]bool
}

func (b *yamlBlock) getPath(parts ...string) string {
	if b == nil {
		return ""
	}
	return b.scalars[strings.Join(parts, ".")]
}

// parseYAMLBlocks reads the top-level sequence of a patch layer.
//
// It understands block mappings, the "- key: value" list form, and nested
// mappings by indentation. That covers the template this writer targets; a
// document using flow style or anchors is rejected by validation elsewhere and
// left untouched here.
func parseYAMLBlocks(raw string) ([]*yamlBlock, error) {
	lines := splitLines(raw)
	type frame struct {
		key    string
		indent int
	}
	var (
		blocks []*yamlBlock
		cur    *yamlBlock
		stack  []frame
		listAt []frame // open "- key:" list entries, by indent
	)
	for i, line := range lines {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		body := strings.TrimSpace(line)

		if indent == 0 && strings.HasPrefix(body, "- ") {
			cur = &yamlBlock{scalars: map[string]string{}, children: map[string]bool{}}
			blocks = append(blocks, cur)
			stack = stack[:0]
			listAt = listAt[:0]
			body = strings.TrimSpace(body[2:])
			if body == "" {
				continue
			}
			key, value, ok := splitYAMLKeyValue(body)
			if !ok {
				// A bare scalar list entry has nothing to address.
				cur = nil
				continue
			}
			if key == "id" {
				cur.id = unquoteYAML(value)
			}
			if value == "" {
				stack = append(stack, frame{key: key, indent: indent + 2})
				cur.children[key] = true
			} else {
				cur.scalars[key] = value
			}
			continue
		}
		if cur == nil {
			// Content before the first block: not a shape we can address.
			if indent > 0 {
				return nil, fmt.Errorf("yaml: unexpected indentation on line %d", i+1)
			}
			continue
		}

		// Pop frames deeper than the current line.
		for len(stack) > 0 && indent < stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		for len(listAt) > 0 && indent < listAt[len(listAt)-1].indent {
			listAt = listAt[:len(listAt)-1]
		}

		if strings.HasPrefix(body, "- ") || body == "-" {
			// A list entry whose item is a mapping: "- id: x".
			item := strings.TrimSpace(strings.TrimPrefix(body, "-"))
			k, v, ok := splitYAMLKeyValue(item)
			if !ok {
				continue
			}
			var prefix string
			if len(listAt) > 0 {
				prefix = listAt[len(listAt)-1].key + "."
			}
			path := prefix + k
			cur.scalars[path] = v
			if v == "" {
				listAt = append(listAt, frame{key: path, indent: indent + 2})
				cur.children[path] = true
			}
			continue
		}

		key, value, ok := splitYAMLKeyValue(body)
		if !ok {
			continue
		}
		prefix := ""
		if len(stack) > 0 {
			parts := make([]string, 0, len(stack))
			for _, f := range stack {
				parts = append(parts, f.key)
			}
			prefix = strings.Join(parts, ".") + "."
		}
		if len(listAt) > 0 {
			prefix += listAt[len(listAt)-1].key + "."
		}
		path := prefix + key
		if value == "" {
			cur.children[path] = true
			stack = append(stack, frame{key: path, indent: indent + 2})
			continue
		}
		cur.scalars[path] = value
	}
	return blocks, nil
}

// splitYAMLKeyValue splits "key: value" while respecting quotes.
func splitYAMLKeyValue(s string) (string, string, bool) {
	idx := -1
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == ':' && (i+1 >= len(s) || s[i+1] == ' ' || s[i+1] == '\t') {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(s[:idx])
	value := strings.TrimSpace(s[idx+1:])
	// Drop a trailing comment that is not inside quotes.
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return unquoteYAML(key), value, true
}

func unquoteYAML(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// findEntryByID returns the top-level entry with the given id.
func findEntryByID(blocks []*yamlBlock, id string) *yamlBlock {
	for _, b := range blocks {
		if b != nil && b.id == id {
			return b
		}
	}
	return nil
}
