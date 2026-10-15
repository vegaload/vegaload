package main

import (
	"encoding/json"
	"github.com/vegaload/vegaload/internal/hosts"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCmdInit_CreatesAllFiles(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd) //nolint:errcheck
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if code := cmdInit(nil); code != 0 {
		t.Fatalf("cmdInit returned exit code %d", code)
	}

	for _, p := range []string{
		filepath.Join(".claude", "skills", "vegaload", "SKILL.md"),
		filepath.Join(".claude", "skills", "vegaload-smoke", "SKILL.md"),
		filepath.Join(".cursor", "rules", "vegaload.mdc"),
		filepath.Join(".cursor", "rules", "vegaload-smoke.mdc"),
		".mcp.json",
		filepath.Join(".cursor", "mcp.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}

	data, err := os.ReadFile(".mcp.json")
	if err != nil {
		t.Fatalf("reading .mcp.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf(".mcp.json is not valid JSON: %v", err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if _, ok := servers["vegaload"]; !ok {
		t.Error("expected a \"vegaload\" entry in .mcp.json's mcpServers")
	}
}

func TestCmdInit_SecondRunSkipsWithoutForce(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd) //nolint:errcheck
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if code := cmdInit(nil); code != 0 {
		t.Fatalf("first cmdInit returned exit code %d", code)
	}
	before, err := os.ReadFile(".mcp.json")
	if err != nil {
		t.Fatalf("reading .mcp.json: %v", err)
	}

	if code := cmdInit([]string{"-output", "json"}); code != 0 {
		t.Fatalf("second cmdInit returned exit code %d", code)
	}
	after, err := os.ReadFile(".mcp.json")
	if err != nil {
		t.Fatalf("reading .mcp.json: %v", err)
	}
	if string(before) != string(after) {
		t.Error("expected a second cmdInit (no -force) to leave .mcp.json unchanged")
	}
}

func TestCmdInit_PreservesExistingMCPServers(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd) //nolint:errcheck
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	existing := `{"mcpServers":{"other-tool":{"command":"other","args":["serve"]}}}`
	if err := os.WriteFile(".mcp.json", []byte(existing), 0o644); err != nil {
		t.Fatalf("writing existing .mcp.json: %v", err)
	}

	if code := cmdInit(nil); code != 0 {
		t.Fatalf("cmdInit returned exit code %d", code)
	}

	data, err := os.ReadFile(".mcp.json")
	if err != nil {
		t.Fatalf("reading .mcp.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf(".mcp.json is not valid JSON: %v", err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if _, ok := servers["other-tool"]; !ok {
		t.Error("expected the pre-existing \"other-tool\" entry to survive")
	}
	if _, ok := servers["vegaload"]; !ok {
		t.Error("expected a new \"vegaload\" entry to be added")
	}
}

func TestCmdInit_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd) //nolint:errcheck
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if code := cmdInit(nil); code != 0 {
		t.Fatalf("first cmdInit returned exit code %d", code)
	}
	if err := os.WriteFile(filepath.Join(".cursor", "rules", "vegaload.mdc"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("overwriting rules file: %v", err)
	}

	if code := cmdInit([]string{"-force"}); code != 0 {
		t.Fatalf("cmdInit -force returned exit code %d", code)
	}
	data, err := os.ReadFile(filepath.Join(".cursor", "rules", "vegaload.mdc"))
	if err != nil {
		t.Fatalf("reading rules file: %v", err)
	}
	if string(data) == "stale" {
		t.Error("expected -force to overwrite the stale rules file")
	}
}

func TestCmdInit_InvalidOutputMode(t *testing.T) {
	if code := cmdInit([]string{"-output", "bogus"}); code == 0 {
		t.Error("expected a non-zero exit code for an invalid -output mode")
	}
}

func inTempDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(cwd) }) //nolint:errcheck
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
}

func TestCmdInit_EditorLimitsTheFiles(t *testing.T) {
	inTempDir(t)
	if code := cmdInit([]string{"-editor", "cursor"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, p := range []string{filepath.Join(".cursor", "rules", "vegaload.mdc"), filepath.Join(".cursor", "mcp.json")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}
	for _, p := range []string{".mcp.json", filepath.Join(".claude", "skills", "vegaload", "SKILL.md")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s should not exist with -editor cursor", p)
		}
	}
}

func TestCmdInit_EditorRejectsUnknown(t *testing.T) {
	inTempDir(t)
	if code := cmdInit([]string{"-editor", "vim"}); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if code := cmdInit([]string{"-editor", " "}); code != 2 {
		t.Fatalf("empty editor: exit %d, want 2", code)
	}
}

func TestCmdInit_EditorAcceptsAList(t *testing.T) {
	inTempDir(t)
	if code := cmdInit([]string{"-editor", "claude-code, cursor"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(".mcp.json"); err != nil {
		t.Errorf(".mcp.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(".cursor", "mcp.json")); err != nil {
		t.Errorf(".cursor/mcp.json: %v", err)
	}
}

func TestCmdInit_StatusChangesNothing(t *testing.T) {
	inTempDir(t)
	if code := cmdInit([]string{"-status"}); code != 1 {
		t.Fatalf("empty project: exit %d, want 1", code)
	}
	entries, _ := os.ReadDir(".")
	if len(entries) != 0 {
		t.Errorf("-status wrote files: %v", entries)
	}
}

func TestCmdInit_StatusAfterInitIsOK(t *testing.T) {
	inTempDir(t)
	if code := cmdInit(nil); code != 0 {
		t.Fatalf("init exit %d", code)
	}
	if code := cmdInit([]string{"-status"}); code != 0 {
		t.Fatalf("status after init: exit %d, want 0", code)
	}
}

func TestCmdInit_StatusSeesOutdatedRulesAndOtherBinary(t *testing.T) {
	inTempDir(t)
	if code := cmdInit([]string{"-editor", "cursor"}); code != 0 {
		t.Fatalf("init exit %d", code)
	}
	rules := filepath.Join(".cursor", "rules", "vegaload.mdc")
	if err := os.WriteFile(rules, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	mcp := filepath.Join(".cursor", "mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"vegaload":{"command":"/elsewhere/vegaload","args":["mcp","serve"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	rows := map[string]string{}
	for _, a := range hosts.ProjectArtifacts() {
		if a.Host != "cursor" {
			continue
		}
		st, _ := hosts.ArtifactStatus(".", a, exe)
		rows[a.RelPath] = st
	}
	if rows[rules] != hosts.StatusOutdated {
		t.Errorf("rules = %q, want outdated", rows[rules])
	}
	if rows[mcp] != hosts.StatusOtherExe {
		t.Errorf("mcp = %q, want other binary", rows[mcp])
	}
	if code := cmdInit([]string{"-status", "-editor", "cursor"}); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	// -status must not have repaired anything.
	if b, _ := os.ReadFile(rules); string(b) != "old" {
		t.Error("-status changed the rules file")
	}
}

func TestCmdInit_StatusEditorOnlyChecksThatEditor(t *testing.T) {
	inTempDir(t)
	if code := cmdInit([]string{"-editor", "cursor"}); code != 0 {
		t.Fatalf("init exit %d", code)
	}
	if code := cmdInit([]string{"-status", "-editor", "cursor"}); code != 0 {
		t.Errorf("cursor only: exit %d, want 0", code)
	}
	if code := cmdInit([]string{"-status"}); code != 1 {
		t.Errorf("all editors: exit %d, want 1", code)
	}
}

func TestCmdInit_StatusFailsWhenArgsAreNotMCPServe(t *testing.T) {
	inTempDir(t)
	if code := cmdInit([]string{"-editor", "cursor"}); code != 0 {
		t.Fatalf("init exit %d", code)
	}
	exe, _ := os.Executable()
	body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"vegaload": map[string]any{"command": exe, "args": []string{"run"}}}})
	if err := os.WriteFile(filepath.Join(".cursor", "mcp.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := cmdInit([]string{"-status", "-editor", "cursor"}); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

// readServers returns the "mcpServers" map of a config file.
func readServers(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return doc.MCPServers
}

func TestCmdInit_LiveDirWritesEnv(t *testing.T) {
	inTempDir(t)
	live := filepath.Join(t.TempDir(), "shared")
	if code := cmdInit([]string{"-live-dir", live}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, p := range []string{".mcp.json", filepath.Join(".cursor", "mcp.json")} {
		entry := readServers(t, p)["vegaload"]
		env, _ := entry["env"].(map[string]any)
		if len(env) != 1 || env["VEGALOAD_CONFIG_DIR"] != live {
			t.Errorf("%s: env = %v, want only VEGALOAD_CONFIG_DIR=%s", p, entry["env"], live)
		}
	}
	// The folder is for the MCP server to create; init must not.
	if _, err := os.Stat(live); err == nil {
		t.Error("init created the live dir")
	}
}

func TestCmdInit_NoLiveDirHasNoEnv(t *testing.T) {
	inTempDir(t)
	if code := cmdInit(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	raw, _ := os.ReadFile(".mcp.json")
	exe, _ := os.Executable()
	b, _ := json.Marshal(exe)
	want := "{\n  \"mcpServers\": {\n    \"vegaload\": {\n      \"command\": " + string(b) +
		",\n      \"args\": [\n        \"mcp\",\n        \"serve\"\n      ]\n    }\n  }\n}\n"
	if string(raw) != want {
		t.Errorf("output without -live-dir changed:\n%s\nwant:\n%s", raw, want)
	}
}

func TestCmdInit_LiveDirRerunMerges(t *testing.T) {
	inTempDir(t)
	litmus := `{"command":"litmus-lite","args":["mcp"],"env":{"LITMUS_LITE_CONFIG_DIR":"/shared"}}`
	existing := `{"mcpServers":{` +
		`"litmus-lite":` + litmus + `,` +
		`"vegaload":{"command":"/old/vegaload","args":["run"],"env":{"KEEP":"me","VEGALOAD_CONFIG_DIR":"/stale"}}},` +
		`"other":true}`
	if err := os.MkdirAll(".cursor", 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(".cursor", "mcp.json")
	if err := os.WriteFile(p, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(t.TempDir(), "shared")

	// No -force: the live dir still lands, command and args are refreshed.
	if code := cmdInit([]string{"-editor", "cursor", "-live-dir", live}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	exe, _ := os.Executable()
	servers := readServers(t, p)
	vl := servers["vegaload"]
	env, _ := vl["env"].(map[string]any)
	if vl["command"] != exe || env["VEGALOAD_CONFIG_DIR"] != live || env["KEEP"] != "me" || len(env) != 2 {
		t.Errorf("vegaload entry = %v", vl)
	}
	if args, _ := vl["args"].([]any); len(args) != 2 || args[0] != "mcp" || args[1] != "serve" {
		t.Errorf("args = %v", vl["args"])
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(litmus), &want)
	if got := servers["litmus-lite"]; !reflect.DeepEqual(got, want) {
		t.Errorf("litmus-lite entry disturbed: %v", got)
	}
	var doc map[string]any
	raw, _ := os.ReadFile(p)
	_ = json.Unmarshal(raw, &doc)
	if doc["other"] != true {
		t.Error("top-level key lost")
	}

	// Same flag again: nothing to change.
	before := string(raw)
	if code := cmdInit([]string{"-editor", "cursor", "-live-dir", live}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if after, _ := os.ReadFile(p); string(after) != before {
		t.Error("second identical run rewrote the file")
	}

	// Without -live-dir (even with -force) the existing env is untouched.
	if code := cmdInit([]string{"-editor", "cursor", "-force"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	env2, _ := readServers(t, p)["vegaload"]["env"].(map[string]any)
	if env2["VEGALOAD_CONFIG_DIR"] != live || env2["KEEP"] != "me" {
		t.Errorf("env without the flag = %v", env2)
	}
	if _, ok := readServers(t, p)["litmus-lite"]; !ok {
		t.Error("litmus-lite entry lost on -force")
	}
}

func TestCmdInit_LiveDirIsMadeAbsolute(t *testing.T) {
	inTempDir(t)
	if code := cmdInit([]string{"-live-dir", filepath.Join("rel", "..", "shared")}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	wd, _ := os.Getwd()
	want := filepath.Join(wd, "shared")
	env, _ := readServers(t, ".mcp.json")["vegaload"]["env"].(map[string]any)
	if got, _ := env["VEGALOAD_CONFIG_DIR"].(string); got != want || !filepath.IsAbs(got) {
		t.Errorf("VEGALOAD_CONFIG_DIR = %q, want %q", got, want)
	}
}

func TestAbsLiveDir_ExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for in, want := range map[string]string{
		"~":           home,
		"~/rt/shared": filepath.Join(home, "rt", "shared"),
	} {
		got, err := absLiveDir(in)
		if err != nil || got != want {
			t.Errorf("absLiveDir(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
