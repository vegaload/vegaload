// Package hosts describes the agent hosts VegaLoad integrates with
// (Cursor, Claude Code, Claude Desktop) in one place: where each host
// keeps its MCP server config, which rules or skill files it reads, and
// how to detect that it is installed.
//
// Both `vegaload init` (which writes these files) and `vegaload doctor`
// (which checks and can repair them) build on this package, so the two
// commands cannot disagree about where a host's files live. It knows
// nothing about the engine, protocols, or reporting.
//
// Supported platforms are macOS, Linux and Windows. Claude Desktop is
// offered on macOS and Windows, because it has no official Linux build.
package hosts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vegaload/vegaload/internal/skills"
)

// Env is the slice of the machine a host adapter needs to answer its
// questions. Tests build one over a temp directory instead of the real
// home directory.
type Env struct {
	OS       string // runtime.GOOS: "darwin", "linux" or "windows"
	Home     string // the user's home directory
	AppData  string // Windows only: the %APPDATA% folder; empty elsewhere
	Dir      string // the project directory being checked
	LookPath func(name string) (string, error)
	// DirExists reports whether a directory exists. Nil means the real
	// file system; tests pass a fake so detection never depends on what
	// happens to be installed on the machine running them.
	DirExists func(path string) bool
}

func (e Env) onPath(name string) bool {
	if e.LookPath == nil {
		return false
	}
	_, err := e.LookPath(name)
	return err == nil
}

func (e Env) dirExists(p string) bool {
	if e.DirExists != nil {
		return e.DirExists(p)
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Scope says whether a file lives in the project or in the user's home.
type Scope string

const (
	ScopeProject Scope = "project"
	ScopeUser    Scope = "user"
)

// MCPConfig is one JSON file that can hold a host's "mcpServers" map.
type MCPConfig struct {
	Host  string
	Scope Scope
	Path  string   // absolute path
	Key   []string // path to the mcpServers object inside the JSON; nil means top level
	// ReadOnly marks files doctor reports on but never rewrites,
	// because VegaLoad does not own their structure.
	ReadOnly bool
}

// Entry is a registered "vegaload" MCP server.
type Entry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// Expand resolves the placeholders Cursor allows in a config
// (${workspaceFolder}, ${userHome}, ${env:NAME}) so doctor can run the
// command the way the host would.
func (e Entry) Expand(env Env, getenv func(string) string) Entry {
	repl := func(s string) string {
		s = strings.ReplaceAll(s, "${workspaceFolder}", env.Dir)
		s = strings.ReplaceAll(s, "${userHome}", env.Home)
		for {
			i := strings.Index(s, "${env:")
			if i < 0 {
				break
			}
			j := strings.Index(s[i:], "}")
			if j < 0 {
				break
			}
			name := s[i+len("${env:") : i+j]
			s = s[:i] + getenv(name) + s[i+j+1:]
		}
		return s
	}
	out := Entry{Command: repl(e.Command)}
	for _, a := range e.Args {
		out.Args = append(out.Args, repl(a))
	}
	if len(e.Env) > 0 {
		out.Env = map[string]string{}
		for k, v := range e.Env {
			out.Env[k] = repl(v)
		}
	}
	return out
}

// ConfigState is what Inspect found in one config file.
type ConfigState struct {
	Config MCPConfig
	Exists bool
	Err    error  // file unreadable, or not valid JSON
	Entry  *Entry // nil when the file has no "vegaload" server
}

// Inspect reads c and reports whether it registers a "vegaload" server.
func Inspect(c MCPConfig) ConfigState {
	st := ConfigState{Config: c}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		if !os.IsNotExist(err) {
			st.Err = err
		}
		return st
	}
	st.Exists = true
	doc := map[string]any{}
	if len(strings.TrimSpace(string(data))) == 0 {
		return st
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		st.Err = fmt.Errorf("not valid JSON: %w", err)
		return st
	}
	cur := doc
	for _, k := range c.Key {
		next, _ := cur[k].(map[string]any)
		if next == nil {
			return st
		}
		cur = next
	}
	servers, _ := cur["mcpServers"].(map[string]any)
	raw, _ := servers["vegaload"].(map[string]any)
	if raw == nil {
		return st
	}
	e := &Entry{}
	if s, ok := raw["command"].(string); ok {
		e.Command = s
	}
	if arr, ok := raw["args"].([]any); ok {
		for _, a := range arr {
			if s, ok := a.(string); ok {
				e.Args = append(e.Args, s)
			}
		}
	}
	if m, ok := raw["env"].(map[string]any); ok {
		e.Env = map[string]string{}
		for k, v := range m {
			if s, ok := v.(string); ok {
				e.Env[k] = s
			}
		}
	}
	st.Entry = e
	return st
}

// RulesFile is an agent rules or skill file a host reads, with the
// content this version of VegaLoad ships for it.
type RulesFile struct {
	Path    string // absolute path
	Content string
}

// Host is one agent host.
type Host struct {
	ID   string
	Name string
	// Supported reports whether the host exists on osName. Nil means everywhere.
	Supported func(osName string) bool
	// Detect reports whether the host looks installed, with the evidence found.
	Detect func(Env) (bool, string)
	// Configs lists every MCP config file the host may read, project scope first.
	Configs func(Env) []MCPConfig
	// Rules lists the rules or skill files the host reads. Nil for hosts without any.
	Rules func(Env) []RulesFile
}

// SupportedOn reports whether h exists on osName.
func (h Host) SupportedOn(osName string) bool {
	return h.Supported == nil || h.Supported(osName)
}

// All returns the built-in hosts in a stable order.
func All() []Host {
	return []Host{cursor(), claudeCode(), claudeDesktop()}
}

// Get returns the built-in host with the given ID.
func Get(id string) (Host, bool) {
	for _, h := range All() {
		if h.ID == id {
			return h, true
		}
	}
	return Host{}, false
}

// Custom returns a host for any MCP-capable tool, pointed at a config
// file the user names. It has no rules files.
func Custom(path string) Host {
	return Host{
		ID:   "custom",
		Name: "custom MCP config",
		Detect: func(Env) (bool, string) {
			if _, err := os.Stat(path); err == nil {
				return true, path
			}
			return false, ""
		},
		Configs: func(Env) []MCPConfig {
			return []MCPConfig{{Host: "custom", Scope: ScopeUser, Path: path}}
		},
	}
}

func cursor() Host {
	return Host{
		ID:   "cursor",
		Name: "Cursor",
		Detect: func(e Env) (bool, string) {
			switch {
			case e.onPath("cursor"):
				return true, "cursor on PATH"
			case e.OS == "darwin" && e.dirExists("/Applications/Cursor.app"):
				return true, "/Applications/Cursor.app"
			case e.dirExists(filepath.Join(e.Home, ".cursor")):
				return true, "~/.cursor"
			}
			return false, ""
		},
		Configs: func(e Env) []MCPConfig {
			return []MCPConfig{
				{Host: "cursor", Scope: ScopeProject, Path: filepath.Join(e.Dir, ".cursor", "mcp.json")},
				{Host: "cursor", Scope: ScopeUser, Path: filepath.Join(e.Home, ".cursor", "mcp.json")},
			}
		},
		Rules: func(e Env) []RulesFile {
			return []RulesFile{
				{Path: filepath.Join(e.Dir, ".cursor", "rules", "vegaload.mdc"), Content: skills.Cursor},
				{Path: filepath.Join(e.Dir, ".cursor", "rules", "vegaload-smoke.mdc"), Content: skills.CursorSmoke},
			}
		},
	}
}

func claudeCode() Host {
	return Host{
		ID:   "claude-code",
		Name: "Claude Code",
		Detect: func(e Env) (bool, string) {
			switch {
			case e.onPath("claude"):
				return true, "claude on PATH"
			case e.dirExists(filepath.Join(e.Home, ".claude")):
				return true, "~/.claude"
			}
			return false, ""
		},
		Configs: func(e Env) []MCPConfig {
			user := filepath.Join(e.Home, ".claude.json")
			return []MCPConfig{
				{Host: "claude-code", Scope: ScopeProject, Path: filepath.Join(e.Dir, ".mcp.json")},
				{Host: "claude-code", Scope: ScopeUser, Path: user, ReadOnly: true},
				{Host: "claude-code", Scope: ScopeUser, Path: user, Key: []string{"projects", e.Dir}, ReadOnly: true},
			}
		},
		Rules: func(e Env) []RulesFile {
			return []RulesFile{
				{Path: filepath.Join(e.Dir, ".claude", "skills", "vegaload", "SKILL.md"), Content: skills.ClaudeCode},
				{Path: filepath.Join(e.Dir, ".claude", "skills", "vegaload-smoke", "SKILL.md"), Content: skills.ClaudeCodeSmoke},
			}
		},
	}
}

func claudeDesktop() Host {
	return Host{
		ID:        "claude-desktop",
		Name:      "Claude Desktop",
		Supported: func(osName string) bool { return osName == "darwin" || osName == "windows" },
		Detect: func(e Env) (bool, string) {
			switch {
			case e.OS == "windows" && e.AppData != "" && e.dirExists(filepath.Join(e.AppData, "Claude")):
				return true, filepath.Join(e.AppData, "Claude")
			case e.dirExists("/Applications/Claude.app"):
				return true, "/Applications/Claude.app"
			case e.dirExists(filepath.Join(e.Home, "Library", "Application Support", "Claude")):
				return true, "~/Library/Application Support/Claude"
			}
			return false, ""
		},
		Configs: func(e Env) []MCPConfig {
			path := filepath.Join(e.Home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
			if e.OS == "windows" {
				appData := e.AppData
				if appData == "" {
					appData = filepath.Join(e.Home, "AppData", "Roaming")
				}
				path = filepath.Join(appData, "Claude", "claude_desktop_config.json")
			}
			return []MCPConfig{{
				Host:  "claude-desktop",
				Scope: ScopeUser,
				Path:  path,
			}}
		},
	}
}

// Artifact is one file `vegaload init` writes into a project.
type Artifact struct {
	Kind    string // "rules" or "mcp"
	Host    string // the host ID that reads this file: "claude-code" or "cursor"
	RelPath string
	Content string // rules content; empty for MCP configs
}

// ProjectArtifacts lists, in a stable order, the files `vegaload init`
// writes for dir. doctor's --fix repairs the same files.
func ProjectArtifacts() []Artifact {
	return []Artifact{
		{Kind: "rules", Host: "claude-code", RelPath: filepath.Join(".claude", "skills", "vegaload", "SKILL.md"), Content: skills.ClaudeCode},
		{Kind: "rules", Host: "claude-code", RelPath: filepath.Join(".claude", "skills", "vegaload-smoke", "SKILL.md"), Content: skills.ClaudeCodeSmoke},
		{Kind: "rules", Host: "cursor", RelPath: filepath.Join(".cursor", "rules", "vegaload.mdc"), Content: skills.Cursor},
		{Kind: "rules", Host: "cursor", RelPath: filepath.Join(".cursor", "rules", "vegaload-smoke.mdc"), Content: skills.CursorSmoke},
		{Kind: "mcp", Host: "claude-code", RelPath: ".mcp.json"},
		{Kind: "mcp", Host: "cursor", RelPath: filepath.Join(".cursor", "mcp.json")},
	}
}

// ProjectEditors lists the host IDs `vegaload init` can set up in a project.
func ProjectEditors() []string { return []string{"claude-code", "cursor"} }

// Artifact status values, as ArtifactStatus returns them.
const (
	StatusOK       = "ok"
	StatusMissing  = "missing"
	StatusOutdated = "outdated" // a rules file that differs from this version's
	StatusOtherExe = "other binary"
	StatusBadArgs  = "wrong args" // an MCP entry that does not run "mcp serve"
	StatusInvalid  = "invalid"
)

// ArtifactStatus checks one project artifact under dir without changing
// anything. exePath is the binary an MCP entry should point at. The
// second result is a short note, or empty.
func ArtifactStatus(dir string, a Artifact, exePath string) (string, string) {
	path := filepath.Join(dir, a.RelPath)
	if a.Kind == "rules" {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return StatusMissing, ""
			}
			return StatusInvalid, err.Error()
		}
		if string(data) != a.Content {
			return StatusOutdated, "differs from the version this binary ships; run init -force to update"
		}
		return StatusOK, ""
	}
	st := Inspect(MCPConfig{Path: path})
	switch {
	case st.Err != nil:
		return StatusInvalid, st.Err.Error()
	case !st.Exists:
		return StatusMissing, ""
	case st.Entry == nil:
		return StatusMissing, "no \"vegaload\" server in the file"
	case st.Entry.Command != exePath:
		return StatusOtherExe, st.Entry.Command
	}
	if len(st.Entry.Args) < 2 || st.Entry.Args[0] != "mcp" || st.Entry.Args[1] != "serve" {
		return StatusBadArgs, "args should be mcp serve; run init -force"
	}
	return StatusOK, ""
}

// WriteResult records what a write did, for text and JSON reporting.
type WriteResult struct {
	Path   string `json:"path"`
	Status string `json:"status"` // "created", "updated", or "skipped (exists)"
}

// WriteFile writes content to path, creating missing parent directories,
// unless path exists and force is false, in which case it reports
// "skipped (exists)" and leaves the file alone.
func WriteFile(path, content string, force bool) (WriteResult, error) {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return WriteResult{Path: path, Status: "skipped (exists)"}, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return WriteResult{}, err
	}
	status := "created"
	if _, err := os.Stat(path); err == nil {
		status = "updated"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Path: path, Status: status}, nil
}

// MergeConfigFile reads path (if it exists), adds or updates a
// "vegaload" MCP server entry pointing at exePath, and writes the result
// back, keeping every other key and server. See skills.MergeMCPServerConfig.
func MergeConfigFile(path, exePath string, force bool) (WriteResult, error) {
	return MergeConfigFileEnv(path, exePath, force, nil)
}

// MergeConfigFileEnv is MergeConfigFile that also sets the given
// environment variables on the entry. See skills.MergeMCPServerConfigEnv
// for how an existing entry and its env are merged.
func MergeConfigFileEnv(path, exePath string, force bool, env map[string]string) (WriteResult, error) {
	var existing []byte
	existed := false
	if data, err := os.ReadFile(path); err == nil {
		existing = data
		existed = true
	} else if !os.IsNotExist(err) {
		return WriteResult{}, err
	}

	merged, changed, err := skills.MergeMCPServerConfigEnv(existing, exePath, force, env)
	if err != nil {
		return WriteResult{}, err
	}
	if !changed {
		return WriteResult{Path: path, Status: "skipped (exists)"}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return WriteResult{}, err
	}
	if err := os.WriteFile(path, merged, 0o644); err != nil {
		return WriteResult{}, err
	}
	status := "created"
	if existed {
		status = "updated"
	}
	return WriteResult{Path: path, Status: status}, nil
}
