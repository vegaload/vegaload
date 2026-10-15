package main

import "github.com/vegaload/vegaload/internal/live"

// openBrowser implements FR-RPT-02's auto-open: a best-effort use of
// the platform's standard "open this file" command. A failure here (no
// GUI, headless CI, xdg-open missing) is reported as a warning by the
// caller, never as a run failure — the report file itself was already
// written successfully either way. It is the same opener as the live view's
// (internal/live), so there is one place that knows each platform's command.
func openBrowser(path string) error { return live.Opener(path) }
