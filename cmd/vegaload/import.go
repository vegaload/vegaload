package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vegaload/vegaload/internal/har"
)

// cmdImport runs `vegaload import <format> ...`. HAR is the first format.
func cmdImport(args []string) int {
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(`Usage: vegaload import har <recording.har> [flags]

Turns a HAR recording (save it from the network tab of your browser) into
a scenario file. Run "vegaload import har -h" for the flags.`)
		if len(args) < 1 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "har":
		return cmdImportHAR(args[1:])
	}
	fmt.Fprintf(os.Stderr, "vegaload import: unknown format %q (want har)\n", args[0])
	return 2
}

func cmdImportHAR(args []string) int {
	fs := flag.NewFlagSet("import har", flag.ContinueOnError)
	out := fs.String("o", "", `scenario file to write (default: the HAR file name with .vl.js), or "-" for the screen`)
	var hosts repeatedFlags
	fs.Var(&hosts, "host", "keep only requests to this host, or host:port (repeatable; default: the main host of the recording)")
	static := fs.Bool("include-static", false, "keep images, fonts, style sheets and scripts")
	third := fs.Bool("include-third-party", false, "keep requests to other sites (analytics, ads, CDNs)")
	max := fs.Int("max", 0, "stop after this many requests (default: no limit)")
	thinkRaw := fs.String("think-time", "recorded", "the sleep() between requests: none, recorded (the gaps in the recording), capped:5s (recorded, at most 5s) or fixed:1s (1s every time). Gaps under 10ms are not written")
	force := fs.Bool("force", false, "overwrite the scenario file if it exists")
	output := fs.String("output", "text", "output mode: text or json")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload import har <recording.har> [flags]")
		fs.PrintDefaults()
	}

	// Flags may come before or after the file name.
	var files []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if err == flag.ErrHelp {
				return 0
			}
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(files) != 1 {
		fmt.Fprintln(os.Stderr, "vegaload import har: want exactly one HAR file")
		fs.Usage()
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintf(os.Stderr, "vegaload import har: -output %q: want text or json\n", *output)
		return 2
	}
	if *max < 0 {
		fmt.Fprintf(os.Stderr, "vegaload import har: -max %d: want zero or more\n", *max)
		return 2
	}
	think, err := har.ParseThinkTime(*thinkRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload import har: -think-time %v\n", err)
		return 2
	}
	in := files[0]

	f, err := os.Open(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload import har: %v\n", err)
		return 1
	}
	defer f.Close()
	res, err := har.Convert(f, filepath.Base(in), har.Options{
		Hosts: []string(hosts), IncludeStatic: *static, IncludeThirdParty: *third, MaxRequests: *max,
		ThinkTime: think,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload import har: %s: %v\n", in, err)
		return 1
	}
	if res.Requests == 0 {
		fmt.Fprintf(os.Stderr, "vegaload import har: no requests left in %s after the filters (%d entries). Try -include-static, -include-third-party or -host.\n", in, res.Total)
		return 1
	}

	path := *out
	if path == "" {
		path = strings.TrimSuffix(filepath.Base(in), filepath.Ext(in)) + ".vl.js"
	}
	if path == "-" {
		fmt.Print(res.Script)
	} else {
		if !*force {
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(os.Stderr, "vegaload import har: %s already exists (use -force to overwrite)\n", path)
				return 1
			}
		}
		if err := os.WriteFile(path, []byte(res.Script), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload import har: %v\n", err)
			return 1
		}
	}

	if *output == "json" {
		doc := map[string]any{
			"path": path, "requests": res.Requests, "total": res.Total, "skipped": res.Skipped,
			"hosts": res.Hosts, "env": res.EnvNames, "todos": res.Todos,
		}
		w := os.Stdout
		if path == "-" {
			w = os.Stderr
		}
		if err := json.NewEncoder(w).Encode(doc); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload import har: %v\n", err)
			return 1
		}
		return 0
	}

	w := os.Stdout
	if path == "-" {
		w = os.Stderr
	}
	fmt.Fprintf(w, "%s: %d of %d requests\n", path, res.Requests, res.Total)
	var why []string
	for k, n := range res.Skipped {
		why = append(why, fmt.Sprintf("  left out %d: %s", n, k))
	}
	sort.Strings(why)
	for _, l := range why {
		fmt.Fprintln(w, l)
	}
	if len(res.EnvNames) > 0 {
		var fl []string
		for _, n := range res.EnvNames {
			fl = append(fl, "-secret-env "+n)
		}
		fmt.Fprintf(w, "  secrets read from the environment: %s\n", strings.Join(fl, " "))
	}
	if res.Todos > 0 {
		fmt.Fprintf(w, "  %d TODO notes in the file: values that may change on every run\n", res.Todos)
	}
	if path != "-" {
		fmt.Fprintf(w, "next: edit %s, then run: vegaload validate %s\n", path, path)
	}
	return 0
}
