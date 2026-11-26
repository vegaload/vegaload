---
title: "Check a scenario before a real run"
description: "`validate` runs the scenario once, with one user and one iteration, and says whether it works."
sidebar_position: 13
---

# Check a scenario before a real run

```
vegaload validate examples/scenarios/crud-flow.vl.js
```

`validate` runs the scenario once, with one user and one iteration, and says
whether it works. It catches a script that will not load (a syntax error, a
missing file, no `iteration()` in Python) and an iteration that fails (a
wrong URL, a thrown error, a host that is not allowed). It makes real
network calls, under the same host rules as `run`, so a host that is not
localhost needs `-allow-target` or `-yes`. Any `check()` results are listed,
and a failed check does not make the scenario invalid. A scenario with
`setup` and `teardown` runs setup, the one iteration with what setup
returned, then teardown. Options the run would refuse make the scenario not
valid. Add `-output json` for a machine-readable result.

| Exit code | Meaning                          |
|-----------|----------------------------------|
| 0         | The scenario loaded and its iteration ran |
| 1         | The scenario is not valid        |
| 2         | Bad usage                        |

From an agent, the `validate_scenario` tool does the same and returns
`valid`, and for an invalid scenario the `stage` (`load`, `setup`,
`iteration` or `teardown`) and `error`.
