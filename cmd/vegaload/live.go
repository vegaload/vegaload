package main

import (
	"context"
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

	base, err := live.Destination(*url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vegaload live:", err)
		return 2
	}
	if reset {
		if _, err := live.Reset(); err != nil {
			fmt.Fprintln(os.Stderr, "vegaload live:", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "vegaload live: made a new project; runs of the old one stay under its old address")
	}
	r, err := live.Resolve(context.Background(), base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vegaload live: %v\n", err)
		return 1
	}
	if r.Hosted && r.ViewKey == "" {
		fmt.Fprintln(os.Stderr, "vegaload live: set VEGALOAD_LIVE_VIEW_KEY to be shown the page address of the project in the environment")
		return 1
	}
	if r.Identity != nil && r.Identity.Created && !reset && !r.Hosted {
		fmt.Fprintln(os.Stderr, "vegaload live: made a project for this machine")
	}
	page := r.PageURL(base)
	fmt.Println(page)
	if n := r.Notice(); n != "" {
		fmt.Fprintln(os.Stderr, "vegaload live:", n)
	}
	if r.Identity != nil {
		fmt.Fprintf(os.Stderr, "identity kept in %s (mode 0600). The push token in it is never shown, and is sent only to %s://%s.\n", r.Identity.Path, base.Scheme, base.Host)
	}
	if *open {
		if err := live.Opener(page); err != nil {
			fmt.Fprintf(os.Stderr, "vegaload live: could not open the browser (%v)\n", err)
			return 1
		}
	}
	return 0
}
