# archive-loadtest

Open-loop load generator for an Ubuntu Archive mirror or cache.
Single static Go binary, stdlib only. Runs on macOS and Linux.

- Downloads `Packages.gz` for the chosen suites/components, builds a pool of `.deb` paths,
  shuffles it, and by default requests **every package at most once** (cache-miss / cache-exhaustion
  workload). With `-distinct N` it instead samples N packages at random with repeats (cache-hit workload).
- Linear ramp from 0 to `-rate` rps over `-ramp`, then holds for `-hold`.
- Open loop: requests fire on schedule regardless of response time. If the client hits
  `-max-inflight`, excess requests are *dropped and counted* (never silently queued).
- Response bodies are streamed to `io.Discard`; nothing touches disk. Body length is checked
  against the index `Size`, so truncated/corrupt proxy responses show up as `size mismatch`.

## Build

```sh
go build -o archive-loadtest .                       # native
GOOS=linux GOARCH=amd64 go build -o archive-loadtest .    # for an Ubuntu x86_64 VM
```

Needs Go >= 1.22 (Ubuntu 24.04: `apt install golang-go`).

## Run

```sh
# Check pool size and expected bandwidth first, no load sent
./archive-loadtest -base http://PROXY/ubuntu -dry-run

# Defaults (laptop-friendly): ramp 0->5 rps over 1m, hold 1m, packages <= 1 MiB (450 unique packages)
./archive-loadtest -base http://PROXY/ubuntu

# Full-scale run: 200 rps, 5m ramp, 2m hold, no size cap (54,000 unique packages)
./archive-loadtest -base http://PROXY/ubuntu -rate 200 -ramp 5m -hold 2m -max-inflight 2000 -max-size 0

# Proxy addressed by IP, indexes fetched from the real archive
./archive-loadtest -base http://10.0.0.5/ubuntu -host archive.ubuntu.com \
  -index-base http://archive.ubuntu.com/ubuntu
```

Cache test: 500 distinct packages of 10-200 MB, sampled at random with repeats. Request count is
still `rate*ramp/2 + rate*hold`; the first request for each package is a cache miss, later ones hit.
`-dry-run` prints the distinct footprint, i.e. how much the proxy must be able to cache.

```sh
./archive-loadtest -base http://PROXY/ubuntu -min-size 10485760 -max-size 209715200 \
  -distinct 500 -rate 1 -ramp 1m -hold 10m -timeout 5m -max-inflight 100
```

Key flags: `-rate`, `-ramp`, `-hold`, `-suites`, `-components`, `-arch`, `-min-size`, `-max-size`,
`-distinct`, `-max-inflight`, `-timeout`, `-seed`, `-out`. See `-h`.

## Sizing a run

`-dry-run` prints the plan and these figures without sending load.

- **Requests** = `rate*ramp/2 + rate*hold`. The ramp is linear, so it averages half the rate.
  Package size and `-distinct` do not change this.
- **Packages.** Without `-distinct`, every request is a different package (the pool must have at
  least that many). With `-distinct N`, N packages are kept and each request picks one at random,
  with repeats, so slightly fewer than N may actually be requested.
- **Cache hits.** The first request for a package is a miss; repeats can be hits. More requests per
  distinct package means a higher hit share.
- **Bandwidth** = average package size * `-rate`. The client downloads the whole body on every
  request, hit or miss, so the cache never reduces what your machine receives.
- **Distinct footprint** is the total size of the distinct packages requested: the minimum cache
  size needed to keep them all.
- **Concurrency** is about `rate * average latency`. If it reaches `-max-inflight`, requests are
  dropped and counted, not queued.

### Choosing settings for a laptop

Keep peak bandwidth under about 70% of your link, so that you measure the proxy and not your own
connection. Budget in MB/s is roughly `link Mbit/s / 8 * 0.7`:

| Link | Budget |
|---|---|
| 50 Mbit/s Wi-Fi | ~4 MB/s |
| 100 Mbit/s | ~9 MB/s |
| 1 Gbit/s wired | ~85 MB/s |

Max rate = budget / average package size. Approximate averages measured on noble amd64:

| `-min-size` .. `-max-size` | Avg package | Max rate at 4 MB/s (50 Mbit/s) |
|---|---|---|
| up to 1 MiB (default) | ~0.15 MB | ~25 rps |
| 10 MiB .. 50 MiB | ~20 MB | ~0.2 rps |
| 10 MiB .. 200 MiB | ~44 MB | ~0.1 rps |

Recommended on 50 Mbit/s Wi-Fi: small packages and repeats, for a cache-hit test.

```sh
./archive-loadtest -base http://PROXY/ubuntu -max-size 1048576 -distinct 200 \
  -rate 10 -ramp 1m -hold 5m -max-inflight 100 -out reports/
```

That is about 3,300 requests at 1-2 MB/s (8-16 Mbit/s, varying with the random package draw) at peak,
comfortably inside the budget.
Large packages cannot reach a useful rate on a slow link; for those, use a wired connection or run
from a machine near the proxy. Run `-dry-run` first and compare the printed peak with your link.

## Reports

A live per-interval table and an end-of-run summary always go to stdout. With `-out DIR`, the run
also writes three files named by start time (reruns never overwrite):

- `report-<UTC time>.html`: single self-contained page (inline CSS and SVG, no JS, works offline,
  follows light/dark mode). Summary cards, charts of request rate (target vs sent/completed/failed),
  latency (p50/p95/p99, TTFB p95), throughput and client concurrency/drops, plus outcome, latency
  and config tables. A banner flags client-side drops and interrupted runs.
- `report-<UTC time>.json`: config (incl. seed, planned requests/bytes), `interrupted` flag, summary
  (counts, outcomes by status/error, total and TTFB latency percentiles, bytes) and the full
  per-interval `timeseries`.
- `report-<UTC time>.csv`: the same time series, one row per interval, for gnuplot/spreadsheets/Grafana.

```sh
./archive-loadtest -base http://PROXY/ubuntu -out reports/
jq '.summary' reports/report-*.json
```

Latency percentiles cover successful (HTTP 200, size-matching) responses only; failures are counted
in `outcomes` and `fail_per_s`. The final partial interval is included, so the series sums to the
totals. The report is also written after Ctrl-C (`"interrupted": true`) but not on a second Ctrl-C.

## Things to know

- **Bandwidth is the real limit.** Defaults (5 rps, packages <= 1 MiB) peak at about 5 MB/s.
  200 rps of average-size debs is roughly 1 Gbit/s or more. `-dry-run` prints the estimate. On a
  laptop/Wi-Fi the link saturates first and latencies then measure your uplink, not the proxy.
  Use `-max-size` to cap per-request size (`0` = no limit).
- **Pool size.** Without `-distinct`, unique-per-request needs `rate*ramp/2 + rate*hold` packages.
  The default suites (noble, noble-updates, noble-security; all components) give well over 54k for
  amd64; add suites such as `jammy,jammy-updates` for longer runs. With `-distinct N` only N packages
  in the size range are needed. The tool refuses to start if the pool is too small.
- **Reruns.** The shuffle is random per run, so repeated runs overlap and the cache may already be
  warm for earlier packages. Use a different `-suites`/`-seed` or flush the cache between runs.
- `-base` indexes are fetched through the proxy by default, which warms those index objects only.
- HTTP/1.1 with keep-alive; `HTTP(S)_PROXY` env vars are ignored; no `Accept-Encoding: gzip`.
- Ctrl-C stops dispatch, drains in-flight requests, prints the summary. A second Ctrl-C aborts.
- Output columns: `target` is the planned mean rps in the window; `sent/s` should track it;
  `infl` is concurrent requests; `drop` is cumulative client-side drops (if non-zero, the
  generator, not the proxy, was the bottleneck).
