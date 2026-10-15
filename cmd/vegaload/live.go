package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/vegaload/vegaload/internal/live"
)

// cmdLive shows or resets the live-view identity of this machine.
//
//	vegaload live           print the project page URL and where it is kept
//	vegaload live -open     also open it in the browser
//	vegaload live reset     start a new project (the old runs stay under the old URL)
func cmdLive(args []string) int {
	fs := flag.NewFlagSet("live", flag.ContinueOnError)
	open := fs.Bool("open", false, "open the project page in the browser")
	url := fs.String("live-url", "", "base address of the live view service (default: VEGALOAD_LIVE_URL, or "+live.DefaultBase+")")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload live [-open] [-live-url URL] | vegaload live reset")
		fmt.Fprintln(fs.Output(), "Prints the address of this machine's live view page. `reset` makes a new project;")
		fmt.Fprintln(fs.Output(), "runs of the old one stay under the old address.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	reset := fs.NArg() == 1 && fs.Arg(0) == "reset"
	if fs.NArg() > 1 || (fs.NArg() == 1 && !reset) {
		fmt.Fprintln(os.Stderr, "vegaload live: usage: live [-open] [-live-url URL] | live reset")
		return 2
	}

	var id *live.Identity
	var err error
	if reset {
		id, err = live.Reset()
	} else {
		id, err = live.Load()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "vegaload live:", err)
		return 1
	}
	base, err := id.BaseURL(*url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vegaload live:", err)
		return 2
	}
	switch {
	case reset:
		fmt.Fprintln(os.Stderr, "vegaload live: made a new project; runs of the old one stay under its old address")
	case id.Created:
		fmt.Fprintln(os.Stderr, "vegaload live: made a project for this machine")
	}
	fmt.Println(id.PageURL(base))
	fmt.Fprintf(os.Stderr, "identity kept in %s (mode 0600). The push token in it is never shown, and is sent only to %s://%s.\n", id.Path, base.Scheme, base.Host)
	if *open {
		if err := live.Opener(id.PageURL(base)); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload live: could not open the browser (%v)\n", err)
			return 1
		}
	}
	return 0
}
