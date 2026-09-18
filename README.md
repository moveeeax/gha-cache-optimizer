# gha-cache-optimizer

[![ci](https://github.com/moveeeax/gha-cache-optimizer/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/gha-cache-optimizer/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Lint GitHub Actions workflows for **missing or ineffective dependency caching** — and print the exact `actions/cache` snippet that fixes each one.

A shocking amount of CI wall-clock is just re-downloading dependencies that never changed. `actions/cache` fixes it, but it's easy to get subtly wrong: a key with no lockfile hash never invalidates, a missing `restore-keys` turns every key miss into a cold start, and a `setup-*` step one line away from a free built-in cache goes unused. Those are silent — the pipeline is green, it's just slower than it should be, forever. This tool makes them loud.

## What it does

Point it at your workflow directory. For every job it finds a dependency install with no cache, an unstable cache key, or a cache step with no fallback — and prints a ready-to-paste fix. It exits `1` when there's a warning, so it works as a CI gate on your own workflows.

## How it works

Each `*.yml` / `*.yaml` file is parsed with `yaml.v3` into a node tree (so every finding carries a real line number). For each job:

1. A first pass records whether the job already covers its deps — an explicit `actions/cache` step, or a `setup-*` action with a built-in `cache:` input (`setup-go` caches by default).
2. A second pass matches `run:` commands against a table of ecosystems (`npm`/`yarn`/`pnpm`, `go`, `pip`/`poetry`, `mvn`, `bundle`). An install with no covering cache is a **`missing-cache`** warning.
3. Existing `actions/cache` steps are checked for a `hashFiles(...)` component in the key (**`unstable-key`**, warning) and for `restore-keys` (**`no-restore-keys`**, info).

The suggested fix is generated from the ecosystem: correct `path`, a key hashed on the right lockfile (`package-lock.json`, `go.sum`, `requirements.txt`, …), and a `restore-keys` prefix.

Unrelated or malformed YAML nearby is skipped, not fatal — a file without a `jobs:` mapping simply isn't a workflow.

## Install

```sh
go install github.com/moveeeax/gha-cache-optimizer/cmd/gha-cache-optimizer@latest
```

Or build from source:

```sh
git clone https://github.com/moveeeax/gha-cache-optimizer.git
cd gha-cache-optimizer
go build ./cmd/gha-cache-optimizer
```

## Usage

```
usage: gha-cache-optimizer [--json] [PATH ...]

  --json   emit JSON instead of text
```

`PATH` defaults to `.github/workflows`. A directory is scanned for `*.yml`/`*.yaml`; individual files are scanned directly.

```sh
$ gha-cache-optimizer examples/needs-cache.yml
WARN  needs-cache.yml:12  [missing-cache]  job="test" step="Install deps"
  node dependency install with no cache — every run re-downloads from scratch
  suggested fix:
    Either enable the setup action's built-in cache:
        - uses: actions/setup-node@v4
          with:
            cache: npm
      or add an explicit cache step:
        - uses: actions/cache@v4
          with:
            path: |
              ~/.npm
            key: ${{ runner.os }}-node-${{ hashFiles('**/package-lock.json') }}
            restore-keys: |
              ${{ runner.os }}-node-

1 file(s) scanned — 1 warning(s), 0 info
```

Add it to your own pipeline as a gate:

```yaml
- run: go run github.com/moveeeax/gha-cache-optimizer/cmd/gha-cache-optimizer@latest .github/workflows
```

## Rules

| Rule | Severity | What it catches |
|------|----------|-----------------|
| `missing-cache`    | WARN | A dependency install (`npm ci`, `go test`, `pip install`, `mvn`, `bundle install`, …) with no cache covering it |
| `unstable-key`     | WARN | An `actions/cache` key with no `hashFiles()` — it never invalidates on dependency change |
| `no-restore-keys`  | INFO | An `actions/cache` step with no `restore-keys` fallback |

## Exit codes

| Code | Meaning |
|------|---------|
| `0`  | No warnings (info-only findings allowed) |
| `1`  | At least one warning — CI gate |
| `2`  | Runtime error (bad path, unreadable file) |

## License

MIT — see [LICENSE](LICENSE).
