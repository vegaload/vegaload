package report

import (
	"strings"
	"testing"
	"time"
)

// numbersResult has every part of the run's numbers that only some runs
// have: failure reasons, iterations, drops and a warning.
func numbersResult() *Result {
	return &Result{
		Executor: "constant-arrival-rate", Elapsed: time.Second, Total: 30, Failed: 4, ErrorRate: 4.0 / 30,
		FailureReasons:    []FailureReason{{Reason: "status 503", Count: 3}, {Reason: "<timeout>", Count: 1}},
		Iterations:        &IterationStats{Total: 10, Failed: 2, ErrorRate: 0.2, Duration: Latency{P50: 60 * time.Millisecond}},
		DroppedIterations: 12,
		Warnings:          []string{"12 of 22 planned iterations (54.55%) were dropped <now>"},
	}
}

func TestHTML_ShowsTheRunsNumbers(t *testing.T) {
	out := renderHTML(numbersResult())
	for _, want := range []string{
		`<span class="n">12</span><span class="l">dropped iterations</span>`,
		`<span class="badge fail">WARNING</span> 12 of 22 planned iterations (54.55%) were dropped &lt;now&gt;`,
		"<h2>Failures</h2>", "<tr><td>status 503</td><td>3</td></tr>", "<tr><td>&lt;timeout&gt;</td><td>1</td></tr>",
		"<h2>Iterations</h2>", `<span class="badge fail">FAIL</span></td><td>10</td><td>2</td><td>20.00%</td><td>60ms</td>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	clean := renderHTML(&Result{Executor: "fixed-vus", Total: 5})
	for _, absent := range []string{"dropped iterations", "WARNING", "<h2>Failures</h2>", "<h2>Iterations</h2>"} {
		if strings.Contains(clean, absent) {
			t.Errorf("a clean run's report has %q", absent)
		}
	}
}

func TestMarkdown_ShowsTheRunsNumbers(t *testing.T) {
	out := MarkdownSummary(numbersResult())
	for _, want := range []string{
		"**Warning:** 12 of 22 planned iterations (54.55%) were dropped <now>",
		"### Failures", "| status 503 | 3 |",
		"### Iterations", "| FAIL | 10 | 2 | 20.00% | 60ms |",
		"12 iterations were dropped because every VU was busy.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	clean := MarkdownSummary(&Result{Executor: "fixed-vus", Total: 5})
	for _, absent := range []string{"Warning", "Failures", "Iterations", "dropped"} {
		if strings.Contains(clean, absent) {
			t.Errorf("a clean run's summary has %q:\n%s", absent, clean)
		}
	}
}
