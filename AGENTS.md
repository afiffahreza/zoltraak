# AGENTS.md

Notes for agents working on `zoltraak`. Read `README.md` for user-facing docs;
this file covers structure, decisions and gotchas.

## What this is

An open-loop HTTP load generator for any server that serves an Ubuntu archive. It reads
`Packages.gz` indexes, builds a pool of `.deb` paths, and requests them on a ramp-then-hold
schedule. Go (>= 1.22), stdlib only, single `package main`, no tests.

## Layout

| File | Role |
|---|---|
| `main.go` | Flags (`parseFlags`, `usage`, `shortcuts`), `validateBase`, `run` (orchestration, dispatch loop), `fetch` (one request), `newClient` |
| `index.go` | `loadPool` (fetch indexes, filter by size, dedupe), `schedule` (unique vs `--distinct` sampling), `shuffle`, `parsePackages` |
| `size.go` | `sizeFlag` (`flag.Value`) and `parseSize`: `--min-size`/`--max-size` accept bytes or K/M/G/T suffixes (K/KiB = 1024, KB = 1000) |
| `pacer.go` | `plan`: ramp/hold math. `at(n)` is when request n is due |
| `stats.go` | Counters, per-interval `sample`s, percentiles, live table, final summary |
| `report.go` | `--out` writer for `report-<UTC>.{json,csv,html}` |
| `html.go` | Self-contained HTML report (inline CSS/SVG, no JS) |

Flow: flags -> `validateBase` -> `loadPool` -> `shuffle` -> `schedule` -> print plan (`--dry-run`
stops here) -> dispatch loop (one goroutine per request, semaphore = `--max-inflight`) -> stats ->
summary -> report.

## Behaviour that is easy to get wrong

- **Open loop.** The dispatch loop sends request n at `plan.at(n)` regardless of response time. A
  slow download only occupies one in-flight slot. If `--max-inflight` is full the request is
  dropped and counted, never queued. `dispatch:` is a label so `break dispatch` can leave the
  loop from inside the `select`.
- **Request count** = `rate*ramp/2 + rate*hold`. It never depends on package size or `--distinct`.
- **Two workloads.** `--distinct 0` (default): every request is a different package, so the pool
  must be at least as big as the request count. `--distinct N`: keep the first N packages of the
  shuffled pool and sample them uniformly with replacement (seeded, so `--seed` replays it). Fewer
  than N may actually be requested.
- **Whole body is always downloaded**, hit or miss. Bandwidth = average package size * rate.
- The tool cannot see cache hits or misses; infer them from latency and TTFB. The target's cache
  is invisible from outside, so do not add header-based hit detection.
- Latency percentiles cover successful (HTTP 200, size-matching) responses only.
- `HTTP_PROXY`/`HTTPS_PROXY` env vars are ignored on purpose (`Transport.Proxy = nil`).

## CLI conventions (decided with the user)

- Flags are documented and printed with `--` (GNU style). Go's `flag` package accepts both `-x` and
  `--x`; do not change the parser. The only single-dash forms are shortcuts: `-t` (`--target`),
  `-i` (`--index`) and `-h` (`--help`). Add shortcuts via the `shortcuts` map in `main.go`;
  `usage()` and `parseFlags` pick them up.
- `--target` is **required** and has no default. `validateBase` also requires an `http(s)://` URL
  with a host. There is deliberately **no** check against `*.ubuntu.com` (it was added, then
  removed at the user's request). Do not re-add it.
- `--index` defaults to `--target`. `--host` only sets the `Host:` header; it does not change where
  requests go. The user runs with the target addressed by IP, `--host archive.ubuntu.com` and
  `--index http://archive.ubuntu.com/ubuntu`.
- Defaults are laptop-friendly on purpose: `--rate 5`, `--ramp 1m`, `--hold 1m`,
  `--max-inflight 100`, `--max-size 1M`. The old defaults (200 rps etc.) are in the README as
  a full-scale example. Keep defaults small.
- Docs wording: call the thing under test "the server" or "the target". Do not call it a
  "proxy" or "mirror". "Cache hit/miss" is fine where it describes the workload. The only
  "proxy" left is the `HTTP_PROXY` env var name.
- Internal names (`cfg.base`, `cfg.indexBase`) and JSON report keys (`base`, `index_base`) were
  intentionally not renamed when the flags became `--target`/`--index`, to keep report
  compatibility.

## Build and verify

```sh
gofmt -l . && go vet ./... && go build -o zoltraak .
./zoltraak -t http://SERVER/ubuntu --dry-run     # safe: only downloads Packages.gz
```

- `--dry-run` fetches only the index files (from `--index`, else `--target`). It sends no package
  requests, so it is the right smoke test.
- The `zoltraak` binary is gitignored; rebuild after any code change.
- There are no tests. Verify behaviour with `--dry-run` and the printed plan numbers.

## Release automation

- `.github/workflows/release.yml` checks formatting, runs `go vet` and ShellCheck, builds
  Linux/macOS amd64/arm64 archives, and smoke-tests the Linux amd64 binary with `--help` on
  PRs targeting `main`.
- Every push to `main`, including PR merges and direct pushes, publishes those archives and
  `SHA256SUMS` with the tag `v0.0.<workflow run number>`. PR checks also consume run numbers,
  so gaps are expected. Tags point to the exact pushed commit; GitHub generates release notes.
- Actions must be enabled and repository/organization policy must allow release creation.
  Only the publishing job has `contents: write`, using the built-in `GITHUB_TOKEN`; no extra
  secret is needed. Require the `build` status check in `main` branch rules to gate merges.
- Each archive contains `zoltraak`, `LICENSE`, and `README.md`. `install.sh` detects the
  platform, resolves the latest release to a fixed tag, verifies its SHA-256 checksum, and
  installs into `~/.local/bin`. Overrides: `INSTALL_DIR` and `ZOLTRAAK_VERSION`.
- The installer requires at least one published release. Checksums detect corruption, not
  independent publisher authenticity. Keep archive names and checksums in sync with the installer.
- Keep CI and release maintenance details here, not in the human-facing README. Its entry
  points are **Quick start** (binary install and first run) and **Build locally** (source build).

## Safety with the user's server

The user tests their own server (it was reached by IP in this session). **Do not send real load
without asking.** An earlier 200 rps run was started and the user stopped it because they feared
overloading it. Run `--dry-run` first, report the planned requests and peak bandwidth, and let the
user choose the rate.

## Sizing reference (measured on noble amd64)

Average package size: about 0.12-0.18 MB with `--max-size 1M`, about 20 MB for 10-50 MiB, about
44 MB for 10-200 MiB. Averages vary with the random draw. Budget about 70% of the link; for a 50
Mbit/s link that is about 4 MB/s. The README's "Choosing settings for a laptop" has the table.

## Docs to keep in sync

If you change a flag, default or the plan math, update `README.md` (flag names in examples, the
"Sizing a run" section, the laptop table) and the help strings in `main.go`.
