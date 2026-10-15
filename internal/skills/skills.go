// Package skills implements FR-MCP-04's agent skill bundles and the
// `vegaload init` command built on them: the actual Markdown content
// Claude Code and Cursor read to know when and how to call VegaLoad's
// MCP tools, embedded into the compiled binary at build time (via
// go:embed) so `vegaload init` can write them out without needing the
// vegaload source tree to be present on the machine it's installed on.
package skills

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// ClaudeCode is the Claude Code skill bundle's SKILL.md content,
// written by Init to .claude/skills/vegaload/SKILL.md.
//
//go:embed bundles/claude-code/SKILL.md
var ClaudeCode string

// Cursor is the Cursor rules bundle's content, written by Init to
// .cursor/rules/vegaload.mdc.
//
//go:embed bundles/cursor/vegaload.mdc
var Cursor string

// ClaudeCodeSmoke is the Claude Code smoke-test skill's SKILL.md content,
// written to .claude/skills/vegaload-smoke/SKILL.md.
//
//go:embed bundles/claude-code-smoke/SKILL.md
var ClaudeCodeSmoke string

// CursorSmoke is the Cursor smoke-test rules content, written to
// .cursor/rules/vegaload-smoke.mdc.
//
//go:embed bundles/cursor/vegaload-smoke.mdc
var CursorSmoke string

// mcpServerEntry is one entry under a Claude Code or Cursor MCP config
// file's "mcpServers" key — both tools share this schema.
type mcpServerEntry struct {
	Command string         `json:"command"`
	Args    []string       `json:"args"`
	Env     map[string]any `json:"env,omitempty"`
}

// MergeMCPServerConfig takes an existing MCP config file's raw JSON
// (nil or empty for "no existing file") and returns the JSON it should
// be replaced with, with a "vegaload" entry added or updated under
// "mcpServers" pointing at exePath — preserving every other key and
// every other configured server untouched. It returns ok=false without
// modifying anything when a "vegaload" entry already exists and force
// is false, so a second `vegaload init` doesn't clobber a hand-edited
// config by default.
func MergeMCPServerConfig(existing []byte, exePath string, force bool) (merged []byte, changed bool, err error) {
	return MergeMCPServerConfigEnv(existing, exePath, force, nil)
}

// MergeMCPServerConfigEnv is MergeMCPServerConfig plus environment
// variables for the server. Each key in env is set (or replaced) in the
// entry's "env" object; env keys already there and not named in env are
// kept, and with a nil or empty env an existing "env" is left as it is.
// The entry has no "env" key at all when nothing sets one, so the output
// without env is the same as MergeMCPServerConfig's always was.
//
// When env is not empty the entry is brought up to date even if it
// exists and force is false: command and args are refreshed and the env
// keys set. Nothing is written (changed is false) when that produces the
// file already on disk.
func MergeMCPServerConfigEnv(existing []byte, exePath string, force bool, env map[string]string) (merged []byte, changed bool, err error) {
	doc := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, false, fmt.Errorf("existing config is not valid JSON: %w", err)
		}
	}

	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	cur, exists := servers["vegaload"]
	if exists && !force && len(env) == 0 {
		return existing, false, nil
	}

	entry := mcpServerEntry{Command: exePath, Args: []string{"mcp", "serve"}}
	if old, ok := cur.(map[string]any); ok {
		if oldEnv, ok := old["env"].(map[string]any); ok && len(oldEnv) > 0 {
			entry.Env = map[string]any{}
			for k, v := range oldEnv {
				entry.Env[k] = v
			}
		}
	}
	for k, v := range env {
		if entry.Env == nil {
			entry.Env = map[string]any{}
		}
		entry.Env[k] = v
	}
	servers["vegaload"] = entry
	doc["mcpServers"] = servers

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("encoding merged config: %w", err)
	}
	out = append(out, '\n')
	if exists && !force && string(out) == string(existing) {
		return existing, false, nil
	}
	return out, true, nil
}
