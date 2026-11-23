---
title: "Explain a run's results"
description: "Keep a run's JSON summary alongside its HTML report with `-out`:"
sidebar_position: 18
---

# Explain a run's results

Keep a run's JSON summary alongside its HTML report with `-out`:

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -vus 10 -duration 30s -out baseline.json
vegaload diagnose baseline.json
```

`diagnose` reports the failure rate and the failures by reason, flags a
latency long tail (p99 more than 5x p50), says whether failures were
concentrated in one period or spread out, and notes dropped iterations and
failed iterations — plus suggested pass/fail thresholds (a p95 ceiling, an error
rate ceiling) derived from that same run, for writing into a CI gate. Add a
bring-your-own-LLM connector for a plain-English narrative on top of the
rule-based findings:

```
export VEGALOAD_LLM_PROVIDER=openai       # or anthropic, ollama
export VEGALOAD_LLM_API_KEY=sk-...
vegaload diagnose baseline.json
```

Nothing about a run or its results leaves your machine unless you configure
this yourself (`-no-llm` skips narration even if it's configured).
