---
title: How it works
description: What vegaload init writes, what the MCP server is, and how an agent runs a test.
sidebar_position: 1
---

# How it works

There are three parts. They are all small.

## 1. The command line

Everything VegaLoad does is a command: `vegaload run`, `validate`, `compare`, `new`, `diagnose`. You can run them by hand. A person and an agent use the same commands, and get the same results.

## 2. The MCP server

`vegaload mcp serve` is a small server that speaks the Model Context Protocol. An agent host such as Cursor or Claude Code starts it and calls its tools.

It has nine tools. Each tool runs the matching command line command with `-output json` and returns the parsed result. There is no agent-only path that skips the command line. See [MCP tools](/ai-agents/mcp-tools).

By default it talks over standard input and output. For use over a network, for example from a container, run `vegaload mcp serve -http 127.0.0.1:8765`. On an address that is not loopback, a token is required.

## 3. The guide files

An agent also needs to know when to use the tools, and how. `vegaload init` writes two short guides for each editor:

- `vegaload`: for real load tests. It covers executors, thresholds, baselines and the safety gate.
- `vegaload-smoke`: for a quick check that an API works. It uses at most 5 users and 10 seconds.

Claude Code reads them as skills in `.claude/skills/`. Cursor reads them as rules in `.cursor/rules/`.

## What `vegaload init` writes

Run it in your project folder.

```bash
vegaload init
```

| File | Used by |
| --- | --- |
| `.claude/skills/vegaload/SKILL.md` | Claude Code |
| `.claude/skills/vegaload-smoke/SKILL.md` | Claude Code |
| `.cursor/rules/vegaload.mdc` | Cursor |
| `.cursor/rules/vegaload-smoke.mdc` | Cursor |
| `.mcp.json` | Claude Code (the server entry) |
| `.cursor/mcp.json` | Cursor (the server entry) |

It does not overwrite a file that already exists. If a `.mcp.json` already has other servers, VegaLoad is added next to them.

Useful options:

```bash
vegaload init -editor cursor          # one editor only
vegaload init -status                 # show what is set up, change nothing
vegaload init -force                  # overwrite what is out of date
vegaload init -live-dir ~/.rt-demo    # MCP server keeps its live view project in this folder
```

`-live-dir DIR` adds `"env": {"VEGALOAD_CONFIG_DIR": "<absolute DIR>"}` to the `vegaload` server entry, so the server and litmus-lite's server can share one live view project. Without it the entry has no `env`.

`-status` shows each file as `ok`, `missing`, `outdated`, `other binary`, `wrong args` or `invalid`. It exits 1 if anything is not `ok`.

## Check the setup

```bash
vegaload doctor
```

Doctor checks the install, each agent host it finds, and whether the MCP server starts and offers all nine tools. See [Cursor](/get-started/cursor) and [Claude Code](/get-started/claude-code).

## A run, step by step

1. You ask: "Load test the cart endpoint."
2. The agent calls `list_protocols`, then `create_scenario` or `generate_from_spec`.
3. It calls `validate_scenario`. This runs the scenario once with one user.
4. It calls `run_test`. VegaLoad checks the target host, runs the test, and writes a JSON result and an HTML report.
5. The agent reads the result. It can call `diagnose_failure`, `suggest_thresholds` or `compare_reports`.
6. A line is added to the audit log. See [Safety](/ai-agents/safety).

## Check that the tool layer works

`vegaload mcp eval` runs a fixed set of tool calls against the tools themselves. It uses no model. Run it in CI after you upgrade VegaLoad.

```bash
vegaload mcp eval
vegaload mcp eval -suite v2
```
