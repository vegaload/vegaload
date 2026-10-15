// init.go implements `vegaload init`: FR-MCP-04's one-command setup for
// an agent-native project. It writes these files, each independently
// skippable if already present (so a second `vegaload init` is a no-op
// unless -force is given):
//
//   - .claude/skills/vegaload/SKILL.md       — Claude Code skill for real load tests
//   - .claude/skills/vegaload-smoke/SKILL.md — Claude Code skill for quick smoke tests
//   - .cursor/rules/vegaload.mdc             — Cursor rules for real load tests
//   - .cursor/rules/vegaload-smoke.mdc       — Cursor rules for quick smoke tests
//   - .mcp.json                              — Claude Code's MCP server config
//   - .cursor/mcp.json                       — Cursor's MCP server config
//
// Both MCP config files point at this same compiled binary (via
// os.Executable()) running `mcp serve`, merged into whatever config
// already exists there rather than overwriting it — see
// internal/skills.MergeMCPServerConfig.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vegaload/vegaload/internal/hosts"
)

// initResult records what cmdInit did for one file, for both its text
// and -output json reporting.
type initResult = hosts.WriteResult

func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project directory to initialize")
	force := fs.Bool("force", false, "overwrite files that already exist")
	output := fs.String("output", "text", "output mode: text or json")
	editor := fs.String("editor", "all", "editors to set up: all, or a comma list of claude-code, cursor")
	status := fs.Bool("status", false, "only report what is set up; change nothing; exit 1 if anything is missing")
	liveDir := fs.String("live-dir", "", "keep the MCP server's live-view identity (live.json) in `DIR`, shared with other tools such as litmus-lite: writes env VEGALOAD_CONFIG_DIR=DIR (made absolute) into the vegaload MCP entry")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload init [flags]")
		fmt.Fprintln(fs.Output(), "Registers VegaLoad's MCP server and skill bundles for this project")
		fmt.Fprintln(fs.Output(), "(Claude Code: .claude/skills/vegaload and vegaload-smoke, .mcp.json; Cursor: .cursor/rules, .cursor/mcp.json).")
		fmt.Fprintln(fs.Output(), "Use -editor to set up only one editor, and -status to see what is set up.")
		fmt.Fprintln(fs.Output(), "Use -live-dir DIR to give the MCP server a shared live-view folder (env VEGALOAD_CONFIG_DIR);")
		fmt.Fprintln(fs.Output(), "it also updates an existing vegaload entry without -force. Terminal runs are not affected.")
		fmt.Fprintln(fs.Output(), "Run \"vegaload doctor\" afterwards to check that everything is wired up.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintf(os.Stderr, "vegaload init: -output %q: want text or json\n", *output)
		return 2
	}

	editors, err := parseEditors(*editor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload init: -editor: %v\n", err)
		return 2
	}

	var mcpEnv map[string]string
	if *liveDir != "" {
		abs, err := absLiveDir(*liveDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vegaload init: -live-dir: %v\n", err)
			return 2
		}
		mcpEnv = map[string]string{"VEGALOAD_CONFIG_DIR": abs}
	}

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload init: locating the vegaload binary: %v\n", err)
		return 1
	}

	var results []initResult
	failed := false

	// The files and their order come from internal/hosts, the same
	// source `vegaload doctor` checks and repairs.
	if *status {
		return initStatus(*dir, exePath, editors, *output)
	}
	for _, a := range hosts.ProjectArtifacts() {
		if !editors[a.Host] {
			continue
		}
		path := filepath.Join(*dir, a.RelPath)
		var res initResult
		var err error
		if a.Kind == "mcp" {
			res, err = hosts.MergeConfigFileEnv(path, exePath, *force, mcpEnv)
		} else {
			res, err = hosts.WriteFile(path, a.Content, *force)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "vegaload init: %s: %v\n", a.RelPath, err)
			failed = true
			continue
		}
		results = append(results, res)
	}

	if *output == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(results); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload init: %v\n", err)
			return 1
		}
	} else {
		for _, r := range results {
			fmt.Printf("%-12s %s\n", r.Status, r.Path)
		}
	}
	if failed {
		return 1
	}
	return 0
}

// absLiveDir makes the -live-dir value absolute. A leading ~ is expanded
// to the home directory in case the shell did not do it (for example
// when the value was quoted). The folder is not created here: the MCP
// server makes it when it first writes live.json.
func absLiveDir(v string) (string, error) {
	if v == "~" || strings.HasPrefix(v, "~/") || strings.HasPrefix(v, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding ~: %v", err)
		}
		v = filepath.Join(home, v[1:])
	}
	return filepath.Abs(v)
}

// parseEditors turns the -editor value into a set of host IDs.
func parseEditors(v string) (map[string]bool, error) {
	set := map[string]bool{}
	for _, part := range strings.Split(v, ",") {
		id := strings.ToLower(strings.TrimSpace(part))
		switch id {
		case "":
		case "all":
			for _, e := range hosts.ProjectEditors() {
				set[e] = true
			}
		case "claude-code", "cursor":
			set[id] = true
		default:
			return nil, fmt.Errorf("%q is not an editor; want all, claude-code or cursor", part)
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("no editor given; want all, claude-code or cursor")
	}
	return set, nil
}

// statusRow is one line of `init -status`.
type statusRow struct {
	Editor string `json:"editor"`
	Path   string `json:"path"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// initStatus reports what init set up, without changing any file.
// It returns 0 when every file is in place and 1 when something is not.
func initStatus(dir, exePath string, editors map[string]bool, output string) int {
	var rows []statusRow
	allOK := true
	for _, a := range hosts.ProjectArtifacts() {
		if !editors[a.Host] {
			continue
		}
		st, note := hosts.ArtifactStatus(dir, a, exePath)
		if st != hosts.StatusOK {
			allOK = false
		}
		rows = append(rows, statusRow{Editor: a.Host, Path: filepath.Join(dir, a.RelPath), Status: st, Note: note})
	}
	if output == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(rows); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload init: %v\n", err)
			return 1
		}
	} else {
		for _, r := range rows {
			line := fmt.Sprintf("%-13s %-13s %s", r.Editor, r.Status, r.Path)
			if r.Note != "" {
				line += "  (" + r.Note + ")"
			}
			fmt.Println(line)
		}
		if allOK {
			fmt.Println("Everything is set up.")
		} else {
			fmt.Println("Something is not set up. Run \"vegaload init -force\" to fix it.")
		}
	}
	if !allOK {
		return 1
	}
	return 0
}
