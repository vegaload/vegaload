package skills

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmbeddedBundlesAreNonEmpty(t *testing.T) {
	if !strings.Contains(ClaudeCode, "name: vegaload") {
		t.Error("expected ClaudeCode to contain its frontmatter name")
	}
	if !strings.Contains(Cursor, "description:") {
		t.Error("expected Cursor to contain its frontmatter description")
	}
}

func TestMergeMCPServerConfig_NoExistingFile(t *testing.T) {
	merged, changed, err := MergeMCPServerConfig(nil, "/usr/local/bin/vegaload", false)
	if err != nil {
		t.Fatalf("MergeMCPServerConfig returned error: %v", err)
	}
	if !changed {
		t.Error("expected changed=true when no config existed")
	}
	var doc map[string]any
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("merged config is not valid JSON: %v", err)
	}
	servers := doc["mcpServers"].(map[string]any)
	entry := servers["vegaload"].(map[string]any)
	if entry["command"] != "/usr/local/bin/vegaload" {
		t.Errorf("command = %v, want /usr/local/bin/vegaload", entry["command"])
	}
	args := entry["args"].([]any)
	if len(args) != 2 || args[0] != "mcp" || args[1] != "serve" {
		t.Errorf("args = %v, want [mcp serve]", args)
	}
}

func TestMergeMCPServerConfig_PreservesOtherServers(t *testing.T) {
	existing := []byte(`{"mcpServers":{"other-tool":{"command":"other","args":["serve"]}}}`)
	merged, changed, err := MergeMCPServerConfig(existing, "/bin/vegaload", false)
	if err != nil {
		t.Fatalf("MergeMCPServerConfig returned error: %v", err)
	}
	if !changed {
		t.Error("expected changed=true")
	}
	var doc map[string]any
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("merged config is not valid JSON: %v", err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if _, ok := servers["other-tool"]; !ok {
		t.Error("expected the existing \"other-tool\" entry to be preserved")
	}
	if _, ok := servers["vegaload"]; !ok {
		t.Error("expected a new \"vegaload\" entry to be added")
	}
}

func TestMergeMCPServerConfig_SkipsExistingWithoutForce(t *testing.T) {
	existing := []byte(`{"mcpServers":{"vegaload":{"command":"/old/path","args":["mcp","serve"]}}}`)
	merged, changed, err := MergeMCPServerConfig(existing, "/new/path", false)
	if err != nil {
		t.Fatalf("MergeMCPServerConfig returned error: %v", err)
	}
	if changed {
		t.Error("expected changed=false when a vegaload entry already exists and force is false")
	}
	if string(merged) != string(existing) {
		t.Error("expected the existing config to be returned unmodified")
	}
}

func TestMergeMCPServerConfig_ForceOverwritesExisting(t *testing.T) {
	existing := []byte(`{"mcpServers":{"vegaload":{"command":"/old/path","args":["mcp","serve"]}}}`)
	merged, changed, err := MergeMCPServerConfig(existing, "/new/path", true)
	if err != nil {
		t.Fatalf("MergeMCPServerConfig returned error: %v", err)
	}
	if !changed {
		t.Error("expected changed=true when force is set")
	}
	var doc map[string]any
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("merged config is not valid JSON: %v", err)
	}
	servers := doc["mcpServers"].(map[string]any)
	entry := servers["vegaload"].(map[string]any)
	if entry["command"] != "/new/path" {
		t.Errorf("command = %v, want /new/path", entry["command"])
	}
}

func TestMergeMCPServerConfig_InvalidExistingJSON(t *testing.T) {
	if _, _, err := MergeMCPServerConfig([]byte("not json"), "/bin/vegaload", false); err == nil {
		t.Error("expected an error for invalid existing JSON")
	}
}

func TestSmokeBundlesStaySmall(t *testing.T) {
	for name, body := range map[string]string{"claude": ClaudeCodeSmoke, "cursor": CursorSmoke} {
		if !strings.Contains(body, "vegaload-smoke") && !strings.Contains(body, "smoke test") {
			t.Errorf("%s: not a smoke bundle", name)
		}
		for _, want := range []string{"vus: 5", `"10s"`, "validate_scenario", "`vegaload`"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", name, want)
			}
		}
		// A smoke bundle must not teach the load-only features.
		for _, banned := range []string{"abort_on_breach", "suggest_thresholds", "compare_reports", "constant-arrival-rate"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s: smoke bundle mentions %q", name, banned)
			}
		}
	}
}

func TestLoadBundlesPointToSmoke(t *testing.T) {
	if !strings.Contains(ClaudeCode, "vegaload-smoke") || !strings.Contains(Cursor, "vegaload-smoke") {
		t.Error("the load bundles should point to the smoke bundles")
	}
	if !strings.Contains(ClaudeCodeSmoke, "name: vegaload-smoke") {
		t.Error("smoke skill needs its frontmatter name")
	}
}

// Every bundle tells the agent to relay the live view's address.
func TestBundlesTellTheAgentToRelayTheLiveViewURL(t *testing.T) {
	for name, body := range map[string]string{"claude": ClaudeCode, "cursor": Cursor, "claude-smoke": ClaudeCodeSmoke, "cursor-smoke": CursorSmoke} {
		for _, want := range []string{"live view: URL", "no-live", "no-open"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", name, want)
			}
		}
	}
}
