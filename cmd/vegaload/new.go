package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/vegaload/vegaload/internal/openapi"
)

const jsTemplate = `// %s — a VegaLoad scenario.
//
// Run it standalone:
//   vegaload run %s
//
// This function runs once per iteration. http and ws are globals every
// VU gets (see AGENTS.md and internal/scripting/js's doc comment): real
// HTTP and WebSocket calls, with the response available to carry into
// the next call -- replace this body with your own flow, e.g.:
//
//   const resp = http.get("http://localhost:8080/widgets");
//   if (!resp.ok) { throw new Error("status " + resp.status); }
//   const widgets = resp.json();
//
// A host outside localhost needs -allow-target or -yes on the run (the
// same FR-CLI-06 allowlist -target uses) or every call to it fails —
// see "vegaload run -h".
export default function () {
  console.log("iteration");
}
`

const pyTemplate = `# %s — a VegaLoad scenario.
#
# Run it standalone:
#   vegaload run %s
#
# This function runs once per iteration. http and ws are globals every
# VU gets (see AGENTS.md and internal/scripting/python's doc comment):
# real HTTP and WebSocket calls, with the response available to carry
# into the next call -- replace this body with your own flow, e.g.:
#
#   resp = http.get("http://localhost:8080/widgets")
#   if not resp.ok:
#       raise ValueError("status %d" % resp.status)
#   widgets = resp.json()
#
# A host outside localhost needs -allow-target or -yes on the run (the
# same FR-CLI-06 allowlist -target uses) or every call to it fails --
# see "vegaload run -h".
def iteration():
    print("iteration")
`

// cmdNew scaffolds a starter scenario file so a new VegaLoad user has
// something real to edit, rather than a blank page — AGENTS.md's first
// rule is that a scenario is always a real file, and this command is
// the fastest way to get one.
func cmdNew(args []string) int {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	python := fs.Bool("python", false, "scaffold a Python scenario instead of JavaScript")
	force := fs.Bool("force", false, "overwrite the file if it already exists")
	output := fs.String("output", "text", "output mode: text or json")
	fromOpenAPI := fs.String("from-openapi", "", "path to a JSON OpenAPI spec; generates a run-command runbook instead of a scenario template")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload new [name] [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintf(os.Stderr, "vegaload new: -output %q: want text or json\n", *output)
		return 2
	}

	name := "scenario"
	if fs.NArg() > 0 {
		name = fs.Arg(0)
	}

	if *fromOpenAPI != "" {
		return cmdNewFromOpenAPI(name, *fromOpenAPI, *force, *output)
	}

	var (
		path     string
		template string
	)
	if *python {
		path = withExt(name, ".py")
		template = pyTemplate
	} else {
		path = withExt(name, ".vl.js")
		template = jsTemplate
	}

	if !*force {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(os.Stderr, "vegaload new: %s already exists (use -force to overwrite)\n", path)
			return 1
		}
	}

	content := fmt.Sprintf(template, path, path)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "vegaload new: %v\n", err)
		return 1
	}

	if *output == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"path": path, "created": true}); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload new: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Printf("created %s\n", path)
	return 0
}

// cmdNewFromOpenAPI implements FR-MCP-06's `vegaload new --from-openapi
// <spec>`: it reads a JSON OpenAPI document at specPath and writes a
// Markdown runbook of ready-to-run `vegaload run` commands, one per
// endpoint the spec declares, rather than one scripted scenario that
// chains them all together. See internal/openapi's doc comment: a flat
// list of per-endpoint protocol-direct commands is what a spec's set of
// independent endpoints actually maps to; a spec alone doesn't say how
// their responses should feed into each other the way a hand-written
// FR-CLI-08 scenario does, so generating one would be guessing at a
// flow the spec never described.
func cmdNewFromOpenAPI(name, specPath string, force bool, output string) int {
	data, err := os.ReadFile(specPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload new: reading %s: %v\n", specPath, err)
		return 1
	}
	spec, err := openapi.Parse(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload new: %v\n", err)
		return 1
	}

	path := withExt(name, ".vegaload-plan.md")
	if !force {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(os.Stderr, "vegaload new: %s already exists (use -force to overwrite)\n", path)
			return 1
		}
	}

	content := spec.RenderRunbook(name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "vegaload new: %v\n", err)
		return 1
	}

	if output == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
			"path": path, "created": true, "endpoints": len(spec.Endpoints),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload new: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Printf("created %s (%d endpoints)\n", path, len(spec.Endpoints))
	return 0
}

// withExt appends ext to name unless name already ends with it (or any
// extension at all) — "scenario" becomes "scenario.vl.js", but
// "myscenario.vl.js" and "myscenario.py" are both left alone.
func withExt(name, ext string) string {
	for _, suffix := range []string{".vl.js", ".vl.ts", ".py", ".js", ".ts"} {
		if len(name) >= len(suffix) && name[len(name)-len(suffix):] == suffix {
			return name
		}
	}
	return name + ext
}
