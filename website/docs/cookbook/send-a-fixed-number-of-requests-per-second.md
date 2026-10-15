---
title: "Send a fixed number of requests per second"
description: "Real traffic does not slow down when your server does."
sidebar_position: 4
---

# Send a fixed number of requests per second

Real traffic does not slow down when your server does. The
`constant-arrival-rate` executor keeps the rate fixed, so a slow server shows
up as a growing delay, not as fewer requests.

```
vegaload run -target http://127.0.0.1:8080/widgets -protocol http1 \
  -executor constant-arrival-rate -rate 50 -max-vus 100 -duration 30s
```

`-rate` is iterations per second. `-max-vus` is the most users VegaLoad may
use to keep that rate.

## Make the rate climb, spike and fall

A flat rate shows whether the target holds one level. To see how it behaves as the load changes, use `ramping-arrival-rate`. `-stages` are `rate:duration` pairs. The rate moves in a straight line from where it was to each target, starting from `-rate` (default 0):

```sh
vegaload run \
  -executor ramping-arrival-rate -max-vus 200 \
  -stages 50:4s,200:4s,600:3s,200:4s,50:3s \
  scenario.vl.js
```

Like `constant-arrival-rate`, it keeps starting iterations on schedule even when the target slows down, and drops an arrival when `-max-vus` are all busy. `-cap-rate` applies to the highest rate in the stages.
