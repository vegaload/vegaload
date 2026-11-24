package report

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// MarkdownSummary renders res as a short Markdown page: the key numbers
// and the verdicts (FR-RPT-05). `vegaload run` appends it to the file
// GitHub Actions names in GITHUB_STEP_SUMMARY, so it shows on the job's
// page. The verdicts are the words PASS and FAIL, not colour or symbols.
func MarkdownSummary(res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## VegaLoad: %s\n\n", mdCell(res.Executor))
	b.WriteString("| Requests | Failed | Error rate | p50 | p95 | p99 | Requests/s |\n|---:|---:|---:|---:|---:|---:|---:|\n")
	rps := 0.0
	if res.Elapsed > 0 {
		rps = float64(res.Total) / res.Elapsed.Seconds()
	}
	fmt.Fprintf(&b, "| %d | %d | %.2f%% | %s | %s | %s | %.1f |\n",
		res.Total, res.Failed, res.ErrorRate*100,
		res.Latency.P50.Round(time.Microsecond), res.Latency.P95.Round(time.Microsecond),
		res.Latency.P99.Round(time.Microsecond), rps)

	if a := res.Aborted; a != nil {
		fmt.Fprintf(&b, "\n**Aborted:** the run was stopped at %s because %s was broken (observed %s).\n",
			a.At.Round(time.Millisecond), mdCell(a.Threshold), mdCell(a.Observed))
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "\n**Warning:** %s\n", mdCell(w))
	}
	if len(res.FailureReasons) > 0 {
		b.WriteString("\n### Failures\n\n| Reason | Requests |\n|---|---:|\n")
		for _, fr := range res.FailureReasons {
			fmt.Fprintf(&b, "| %s | %d |\n", mdCell(fr.Reason), fr.Count)
		}
	}
	if it := res.Iterations; it != nil {
		fmt.Fprintf(&b, "\n### Iterations\n\nThe numbers above are each call the scenario made.\n\n| Result | Ran | Failed | Error rate | p50 | p95 | Max |\n|---|---:|---:|---:|---:|---:|---:|\n| %s | %d | %d | %.2f%% | %s | %s | %s |\n",
			passWord(it.Failed == 0), it.Total, it.Failed, it.ErrorRate*100,
			it.Duration.P50.Round(time.Microsecond), it.Duration.P95.Round(time.Microsecond), it.Duration.Max.Round(time.Microsecond))
	}
	if res.DroppedIterations > 0 {
		fmt.Fprintf(&b, "\n%d iterations were dropped because every VU was busy. They are not in the numbers above.\n", res.DroppedIterations)
	}
	if len(res.DataFiles) > 0 {
		b.WriteString("\n### Data files\n\n")
		for _, f := range res.DataFiles {
			fmt.Fprintf(&b, "- %s (%d rows)\n", mdCell(f.Name), f.Rows)
		}
	}
	if len(res.Thresholds) > 0 {
		verdict := "all passed"
		if res.ThresholdsPassed == nil || !*res.ThresholdsPassed {
			verdict = "breached"
		}
		fmt.Fprintf(&b, "\n### Thresholds: %s\n\n| Result | Threshold | Observed |\n|---|---|---|\n", verdict)
		for _, t := range res.Thresholds {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", passWord(t.Passed), mdCell(t.Name), mdCell(t.Observed))
		}
	}
	if len(res.Checks) > 0 {
		rate, _ := res.CheckRate()
		fmt.Fprintf(&b, "\n### Checks: %.2f%% passed\n\n| Result | Check | Passed | Failed |\n|---|---|---:|---:|\n", rate*100)
		for _, c := range res.Checks {
			fmt.Fprintf(&b, "| %s | %s | %d | %d |\n", passWord(c.Fails == 0), mdCell(c.Name), c.Passes, c.Fails)
		}
	}
	if len(res.Steps) > 0 {
		b.WriteString("\n### Steps\n\n| Result | Step | Runs | Failed | Error rate | p50 | p95 | Max |\n|---|---|---:|---:|---:|---:|---:|---:|\n")
		for _, st := range res.Steps {
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %.2f%% | %s | %s | %s |\n", passWord(st.Failed == 0), mdCell(st.Name), st.Total, st.Failed,
				st.ErrorRate*100, st.Latency.P50.Round(time.Microsecond), st.Latency.P95.Round(time.Microsecond), st.Latency.Max.Round(time.Microsecond))
		}
	}
	if rows := Endpoints(res); len(rows) > 0 {
		b.WriteString("\n### Endpoints\n\n| Result | Endpoint | Calls | Failed | Error rate | p50 | p95 | Max |\n|---|---|---:|---:|---:|---:|---:|---:|\n")
		for _, op := range rows {
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %.2f%% | %s | %s | %s |\n", passWord(op.Failed == 0), mdCell(op.Name), op.Total, op.Failed,
				op.ErrorRate*100, op.Latency.P50.Round(time.Microsecond), op.Latency.P95.Round(time.Microsecond), op.Latency.Max.Round(time.Microsecond))
		}
	}
	if ShowProtocols(res) {
		b.WriteString("\n### Protocols\n\n| Result | Protocol | Calls | Failed | Error rate | p50 | p95 | Max | Sent | Received |\n|---|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, p := range res.Protocols {
			writeMDCall(&b, p.Name, p.Total, p.Failed, p.ErrorRate, p.Latency, p.BytesSent, p.BytesReceived)
			if IsHTTP(p.Name) {
				continue
			}
			for _, op := range p.Operations {
				writeMDCall(&b, "↳ "+op.Name, op.Total, op.Failed, op.ErrorRate, op.Latency, op.BytesSent, op.BytesReceived)
			}
		}
		for _, p := range res.Protocols {
			if len(p.Measures) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n**%s phases**\n\n| Phase | Count | p50 | p95 | Max |\n|---|---:|---:|---:|---:|\n", mdCell(p.Name))
			for _, m := range p.Measures {
				fmt.Fprintf(&b, "| %s | %d | %s | %s | %s |\n", mdCell(m.Name), m.Count,
					m.Latency.P50.Round(time.Microsecond), m.Latency.P95.Round(time.Microsecond), m.Latency.Max.Round(time.Microsecond))
			}
		}
	}
	if bl := res.Baseline; bl != nil {
		verdict := "within the baseline"
		if !bl.Passed {
			verdict = "worse than the baseline"
		}
		fmt.Fprintf(&b, "\n### Baseline: %s\n\nCompared with `%s`, up to %g%% worse allowed.\n\n| Result | Metric | Baseline | This run |\n|---|---|---:|---:|\n",
			verdict, mdCell(bl.Path), bl.MaxRegressionPercent)
		for _, m := range bl.Metrics {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", passWord(!m.Regressed), mdCell(m.Name), baselineValue(m.Baseline, m.Unit), baselineValue(m.Candidate, m.Unit))
		}
		for _, n := range bl.Notes {
			fmt.Fprintf(&b, "\n- %s\n", mdCell(n))
		}
	}
	return b.String()
}

// AppendStepSummary appends MarkdownSummary(res) to the file at path. A
// step summary file is shared by every step of a job, so it is appended
// to, never replaced.
func AppendStepSummary(path string, res *Result) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(MarkdownSummary(res) + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeMDCall(b *strings.Builder, name string, total, failed int64, rate float64, lat Latency, sent, received int64) {
	fmt.Fprintf(b, "| %s | %s | %d | %d | %.2f%% | %s | %s | %s | %s | %s |\n",
		passWord(failed == 0), mdCell(name), total, failed, rate*100,
		lat.P50.Round(time.Microsecond), lat.P95.Round(time.Microsecond), lat.Max.Round(time.Microsecond),
		FormatByteCount(sent), FormatByteCount(received))
}

func passWord(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// mdCell makes s safe inside a Markdown table cell: a pipe would start a
// new cell, and a line break would end the row.
func mdCell(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	return strings.ReplaceAll(s, "|", `\|`)
}
