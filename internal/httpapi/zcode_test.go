package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zcodeFixture is a trimmed but structurally faithful provider_config.json:
// the fields the writer touches are all present, plus a foreign provider that
// must survive untouched.
const zcodeFixture = `{
  "schemaVersion": 1,
  "config": {
    "providerOrder": ["foreign-id", "new-provider-3"],
    "providerConfigRules": {
      "providerRules": [
        {
          "providerId": "foreign-id",
          "providerName": "wp",
          "config": {
            "group": "standard-personal",
            "access": { "type": "api-key", "apiKey": "sk-foreign" },
            "api": { "type": "openai-chat-completions", "baseUrl": "https://example.invalid/v1" },
            "personalModelIds": ["kimi-k3", "glm-5.3-flash"],
            "modelOrder": ["kimi-k3", "glm-5.3-flash"]
          }
        },
        {
          "providerId": "new-provider-3",
          "providerName": "mimo",
          "config": {
            "group": "standard-personal",
            "access": { "type": "api-key", "apiKey": "sk-stale" },
            "api": { "type": "anthropic-messages", "baseUrl": "http://127.0.0.1:8792" },
            "personalModelIds": ["mimo-v2.6-pro"],
            "modelOrder": ["mimo-v2.6-pro"]
          }
        }
      ]
    },
    "modelConfigRules": { "kept": true }
  }
}`

func decodeZCode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	return doc
}

func zcodeRules(t *testing.T, doc map[string]any) []any {
	t.Helper()
	cfg := doc["config"].(map[string]any)
	return cfg["providerConfigRules"].(map[string]any)["providerRules"].([]any)
}

func zcodeEntry(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	for _, r := range zcodeRules(t, doc) {
		m := r.(map[string]any)
		if m["providerId"] == zcodeProviderID {
			return m
		}
	}
	t.Fatal("relay provider entry not found")
	return nil
}

func TestZCodeAddsProviderAndKeepsForeignOnes(t *testing.T) {
	e, _ := tempEnv(t)
	out, err := configureZCodeProviders(zcodeFixture, e, testRelay, agentToken)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	doc := decodeZCode(t, out)

	rules := zcodeRules(t, doc)
	if len(rules) != 3 {
		t.Fatalf("providerRules = %d, want 3", len(rules))
	}

	// The foreign provider must be byte-for-byte equivalent after a round trip.
	var before, after map[string]any
	json.Unmarshal([]byte(zcodeFixture), &before)
	json.Unmarshal([]byte(out), &after)
	wantForeign := before["config"].(map[string]any)["providerConfigRules"].(map[string]any)["providerRules"].([]any)[0]
	gotForeign := after["config"].(map[string]any)["providerConfigRules"].(map[string]any)["providerRules"].([]any)[0]
	if !equalJSON(t, wantForeign, gotForeign) {
		t.Errorf("the foreign provider changed:\n got %v\nwant %v", gotForeign, wantForeign)
	}

	entry := zcodeEntry(t, doc)
	if entry["providerName"] != zcodeProviderName {
		t.Errorf("providerName = %v", entry["providerName"])
	}
	c := entry["config"].(map[string]any)
	if got := c["api"].(map[string]any)["type"]; got != "openai-chat-completions" {
		t.Errorf("api.type = %v", got)
	}
	if got := c["api"].(map[string]any)["baseUrl"]; got != testRelay+"/v1" {
		t.Errorf("api.baseUrl = %v", got)
	}
	if got := c["access"].(map[string]any)["apiKey"]; got != agentToken {
		t.Errorf("apiKey = %v", got)
	}
	ids := c["personalModelIds"].([]any)
	if len(ids) != 1 || ids[0] != defaultAgentModel {
		t.Errorf("personalModelIds = %v", ids)
	}
	if c["group"] != "standard-personal" {
		t.Errorf("group = %v", c["group"])
	}
	// Unrelated sibling keys survive.
	if cfg := doc["config"].(map[string]any); cfg["modelConfigRules"] == nil {
		t.Error("modelConfigRules was dropped")
	}
}

func TestZCodeProviderOrderGetsTheIdOnce(t *testing.T) {
	e, _ := tempEnv(t)
	out, err := configureZCodeProviders(zcodeFixture, e, testRelay, agentToken)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	doc := decodeZCode(t, out)
	order := doc["config"].(map[string]any)["providerOrder"].([]any)
	if len(order) != 3 {
		t.Fatalf("providerOrder = %v, want 3 entries", order)
	}
	if order[0] != "foreign-id" {
		t.Errorf("existing order changed: %v", order)
	}
	if order[2] != zcodeProviderID {
		t.Errorf("relay id not appended: %v", order)
	}

	// A second run must not append the id again.
	out2, err := configureZCodeProviders(out, e, testRelay, agentToken)
	if err != nil {
		t.Fatalf("second configure: %v", err)
	}
	order2 := decodeZCode(t, out2)["config"].(map[string]any)["providerOrder"].([]any)
	if len(order2) != 3 {
		t.Errorf("order grew on re-run: %v", order2)
	}
}

func TestZCodeIdempotent(t *testing.T) {
	e, _ := tempEnv(t)
	once, err := configureZCodeProviders(zcodeFixture, e, testRelay, agentToken)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	twice, err := configureZCodeProviders(once, e, testRelay, agentToken)
	if err != nil {
		t.Fatalf("second configure: %v", err)
	}
	if once != twice {
		t.Errorf("not byte-idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

func TestZCodeRefreshesStaleRelayEntry(t *testing.T) {
	e, _ := tempEnv(t)
	// An entry the relay already owns must be updated in place, keeping its
	// position rather than being appended as a duplicate.
	seeded := strings.Replace(zcodeFixture, `"providerId": "foreign-id"`,
		`"providerId": "`+zcodeProviderID+`"`, 1)
	out, err := configureZCodeProviders(seeded, e, "http://127.0.0.1:9999", "fresh-token")
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	doc := decodeZCode(t, out)
	if got := len(zcodeRules(t, doc)); got != 2 {
		t.Fatalf("providerRules = %d, want 2 (refresh, not append)", got)
	}
	entry := zcodeEntry(t, doc)
	c := entry["config"].(map[string]any)
	if got := c["api"].(map[string]any)["baseUrl"]; got != "http://127.0.0.1:9999/v1" {
		t.Errorf("baseUrl not refreshed: %v", got)
	}
	if got := c["access"].(map[string]any)["apiKey"]; got != "fresh-token" {
		t.Errorf("apiKey not refreshed: %v", got)
	}
}

func TestZCodeFromEmptyDocument(t *testing.T) {
	e, _ := tempEnv(t)
	out, err := configureZCodeProviders("", e, testRelay, agentToken)
	if err != nil {
		t.Fatalf("configure on empty input: %v", err)
	}
	doc := decodeZCode(t, out)
	if doc["schemaVersion"] == nil {
		t.Error("schemaVersion missing")
	}
	if got := len(zcodeRules(t, doc)); got != 1 {
		t.Errorf("providerRules = %d, want 1", got)
	}
}

func TestZCodeRefusesCorruptShapes(t *testing.T) {
	e, _ := tempEnv(t)
	cases := map[string]string{
		"providerRules not an array":        `{"config":{"providerConfigRules":{"providerRules":{"a":1}}}}`,
		"providerConfigRules not an object": `{"config":{"providerConfigRules":[]}}`,
		"config not an object":              `{"config":[]}`,
		"broken json":                       `{"config":`,
	}
	for name, raw := range cases {
		if _, err := configureZCodeProviders(raw, e, testRelay, agentToken); err == nil {
			t.Errorf("%s: expected an error, got a rewrite", name)
		}
	}
}

func TestZCodeWriterIsReachableThroughTheAgentTable(t *testing.T) {
	e, _ := tempEnv(t)
	root := filepath.Join(e.home, ".zcode")
	if err := os.MkdirAll(filepath.Join(root, "v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zcodeProviderPath(e), []byte(zcodeFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configureAgent(e, agentZCode, testRelay, agentToken); err != nil {
		t.Fatalf("configureAgent(zcode): %v", err)
	}
	got, err := os.ReadFile(zcodeProviderPath(e))
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeZCode(t, string(got))
	entry := zcodeEntry(t, doc)
	c := entry["config"].(map[string]any)
	if got := c["api"].(map[string]any)["baseUrl"]; got != testRelay+"/v1" {
		t.Errorf("baseUrl = %v", got)
	}
	// The write path must have taken a backup of the pre-existing file.
	backups, _ := filepath.Glob(zcodeProviderPath(e) + ".bak-*")
	if len(backups) == 0 {
		t.Error("no backup was taken")
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
