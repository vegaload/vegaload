---
title: "Supported protocols"
description: "`vegaload protocols` prints this list."
sidebar_position: 1
---

# Supported protocols

`vegaload protocols` prints this list. `vegaload protocols NAME` prints one
protocol, including its options. `vegaload protocols -output json` prints the
same data as JSON. Example paths are relative to a checkout of this repository.
A released binary does not include the examples folder.

- [Scenarios](/guides/scenarios). `vegaload new my-scenario` scaffolds `my-scenario.vl.js` (JavaScript by default; `-python` for a `.py` scenario shelling out to a local `python3`).
- [Check a scenario](/guides/check-a-scenario). `validate` runs the scenario once, with one user and one iteration, and says whether it works.
- [Data files](/guides/data-files). A scenario can read rows from a file and settings from the environment, so each virtual user can act on different data.
- [Start from a browser recording](/guides/import-har). Record a real session in your browser (the network tab can save it as a HAR file), then turn it into a scenario:
- [Thresholds](/guides/thresholds). Add `-threshold` to make a run pass or fail on its own numbers, for example in CI:
- [Compare runs](/guides/compare-runs). After another run with `-out candidate.json`:
- [Merge results](/guides/merge-results). Combine the JSON reports of runs that sent load at the same time from several machines into one report.
- [Serve the live numbers at /stats](/guides/stats-endpoint). `-stats-addr 127.0.0.1:9093` serves the run's numbers as JSON while it runs, in the shape of the Harness k6 stats sidecar.
- [Explain a run](/guides/explain-a-run). Keep a run's JSON summary alongside its HTML report with `-out`:

<!-- vegaload:protocols -->
| Protocol | Since | URL schemes | Example |
| --- | --- | --- | --- |
| [http1](/guides/http) | 0.1.0 | http, https | [crud-flow.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/crud-flow.vl.js) |
| [http2](/guides/http) | 0.1.0 | https | [crud-flow.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/crud-flow.vl.js) |
| [grpc](/guides/grpc) | 0.1.0 | grpc, grpcs | (none yet) |
| [websocket](/guides/websocket) | 0.1.0 | ws, wss | [http-ws-chain.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/http-ws-chain.vl.js) |
| [mqtt](/guides/mqtt) | 0.6.0 | mqtt, mqtts | [mqtt-tcp.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/mqtt-tcp.vl.js) |
| [kafka](/guides/kafka) | 0.6.0 | kafka, kafkas | [kafka-orders.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/kafka-orders.vl.js) |
| [postgres](/guides/postgres) | 0.7.0 | postgres, postgresql | [postgres-orders.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/postgres-orders.vl.js) |
| [mysql](/guides/mysql) | 0.7.0 | mariadb, mysql | [mysql-orders.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/mysql-orders.vl.js) |
| [redis](/guides/redis) | 0.7.0 | redis, rediss | [redis-cache.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/redis-cache.vl.js) |
| [rabbitmq](/guides/rabbitmq) | 0.7.0 | amqp, amqps | [rabbitmq-orders.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/rabbitmq-orders.vl.js) |
| [ftp](/guides/ftp) | 0.7.0 | ftp, ftps | [ftp-transfer.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/ftp-transfer.vl.js) |
| [sftp](/guides/sftp) | 0.9.0 | sftp | [sftp-transfer.vl.js](https://github.com/vegaload/vegaload/blob/main/examples/scenarios/sftp-transfer.vl.js) |
| [tcp](/guides/tcp-udp) | 0.6.0 | tcp | (none yet) |
| [udp](/guides/tcp-udp) | 0.6.0 | udp | (none yet) |
<!-- /vegaload:protocols -->
