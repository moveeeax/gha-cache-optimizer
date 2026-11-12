# gha-cache-optimizer

[![ci](https://github.com/moveeeax/gha-cache-optimizer/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/gha-cache-optimizer/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Lint GitHub Actions workflows for **missing or ineffective dependency caching** — and print the exact `actions/cache` snippet that fixes each one. Two subcommands read a repository's run history instead: `cache-report` shows the real cache hit rate per workflow, job and key prefix, and `cost` estimates runner minutes and list-price cost per workflow, job and matrix leg.

A shocking amount of CI wall-clock is just re-downloading dependencies that never changed. `actions/cache` fixes it, but it's easy to get subtly wrong: a key with no lockfile hash never invalidates, a missing `restore-keys` turns every key miss into a cold start, and a `setup-*` step one line away from a free built-in cache goes unused. Those are silent — the pipeline is green, it's just slower than it should be, forever. This tool makes them loud.

## What it does

Point it at your workflow directory. For every job it finds a dependency install with no cache, an unstable cache key, or a cache step with no fallback — and prints a ready-to-paste fix. It exits `1` when there's a warning, so it works as a CI gate on your own workflows.

## How it works

Each `*.yml` / `*.yaml` file is parsed with `go.yaml.in/yaml/v3` into a node tree (so every finding carries a real line number). For each job:

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
       gha-cache-optimizer cache-report --repo OWNER/NAME [flags]
       gha-cache-optimizer cost --repo OWNER/NAME [flags]

  --json   emit JSON instead of text
```

`PATH` defaults to `.github/workflows`. A directory is scanned for `*.yml`/`*.yaml`; individual files are scanned directly.

```sh
$ gha-cache-optimizer examples/needs-cache.yml
WARN  needs-cache.yml:12  [missing-cache]  job="test" step="Install deps"
  node dependency install with no cache — every run re-downloads from scratch
  suggested fix:
    Either enable the setup action's built-in cache:
        - uses: actions/setup-node@v7
          with:
            cache: npm
      or add an explicit cache step:
        - uses: actions/cache@v6
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

`cache-report` and `cost` are subcommand names, so a workflow path literally named `cost` or `cache-report` has to be passed as `./cost`.

## Run history: `cache-report` and `cost`

The linter sees what a workflow says; these two read what its runs did. Both call the GitHub REST API with a token from `GITHUB_TOKEN` or `GH_TOKEN` (for example `GH_TOKEN=$(gh auth token)`), use `GITHUB_API_URL` when set (GitHub Enterprise Server, or inside a runner), and take the same flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--repo OWNER/NAME` | `$GITHUB_REPOSITORY` | Repository to read |
| `--since` | `30d` | Window start: `30d`, `2w`, `12h` or `YYYY-MM-DD` |
| `--group-by` | see below | Comma list of grouping columns |
| `--format` | `table` | `table`, `csv` or `json` |
| `--workflow` | all | Only runs of one workflow file, e.g. `ci.yml` |
| `--max-runs` | `200` (cache-report), `0` (cost) | Only the newest N runs in the window (`0` = all) |
| `--workers` | `4` | Parallel API requests |
| `--cache-dir` | user cache dir | Where completed-run data is kept between invocations |
| `--no-disk-cache` | off | Neither read nor write that directory |

Only completed runs are read, and jobs come from every run attempt (`filter=all`), so re-runs count. The runs API returns at most 1,000 results for a `created` window; a window above that is split in halves until each piece fits. Jobs of a completed run attempt, and the cache events parsed from a job log, never change, so they are stored under `<user cache dir>/gha-cache-optimizer/OWNER/NAME/` and a repeated report only fetches new runs. Both subcommands exit `0` on success and `2` on any error.

### `cache-report`

```sh
gha-cache-optimizer cache-report --repo owner/example --since 30d --group-by key-prefix --format table
```

For every job in the window it downloads the job log and finds each cache restore written by `actions/cache` (including `actions/cache/restore`) and by the built-in caches of `setup-node`, `setup-go`, `setup-python` and `setup-java`:

- **hit**: restored under the exact primary key;
- **partial**: a `restore-keys` prefix matched an older entry, so the job starts warm but saves a new entry;
- **miss**: nothing matched (`Cache not found for input keys: …`, `… cache is not found`).

Restore time is measured from the step's first log line to the restore result; save time from the post step's start to the `Cache saved …` line. For `setup-*` actions the restore time includes downloading the toolchain, because both happen in the same step. The key prefix is the key up to its first hash (`Linux-node-3f2a…` → `Linux-node-`); a `setup-*` miss without a later save has no key in the log and is grouped as `(actions/setup-go)`.

`--group-by` takes any of `workflow`, `job`, `key-prefix` (default: all three; `job` is the job name without its matrix suffix). `--view` picks the table:

| View | Rows |
|------|------|
| `summary` (default) | One per group: restores, hits, partial, misses, hit rate, average restore and save seconds, and the date of the last exact hit. Worst hit rate first, so a prefix that has said `never` since March is at the top. |
| `trend` | One per group per ISO week (Monday, UTC), to find the week a prefix stopped hitting. |
| `entries` | Live cache entries from the Actions cache API (key, ref, size, last access). Reads no run history. |

Text output ends with the repository's cache storage against its size limit. GitHub's default limit is 10 GB per repository and it can be raised in the Actions settings, so pass `--cache-limit-gb` if yours differs. Job logs past their retention period are skipped and counted in the footer.

### `cost`

```sh
gha-cache-optimizer cost --repo owner/example --since 30d --group-by matrix --format csv
```

Each job is priced from its `runs-on` labels: execution time (`started_at` → `completed_at`) is rounded up to a whole minute, as GitHub bills each job, and multiplied by the per-minute list price of the runner SKU. Queue time (`created_at` → `started_at`) is reported in its own column and not billed.

`--group-by` takes any of `workflow`, `job`, `matrix` (the full job name, so each matrix leg is a row) and `runner` (the SKU); default `workflow,matrix`. Rows are sorted by cost, most expensive first, with a `TOTAL` row.

Built-in prices are from GitHub's [Actions runner pricing](https://docs.github.com/en/billing/reference/actions-runner-pricing) page as of 2026-11-12, for example Linux 2-core `ubuntu-*` $0.006/min, `ubuntu-*-arm` $0.005, `ubuntu-slim` $0.002, `windows-*` $0.010, `macos-*` $0.062, `macos-*-large` $0.077, `macos-*-xlarge` $0.102. Larger runners use labels each organization picks, so map them with `--rate` (repeatable), either to a price or to a SKU from that page:

```sh
gha-cache-optimizer cost --repo owner/example --rate big-linux=linux_8_core --rate gpu=0.052
```

Jobs on `self-hosted` runners show their minutes but no cost. Jobs whose labels match nothing are listed under `unpriced:<labels>` with a hint to add `--rate`. The figure is the list price: free minutes for public repositories and the minutes included in a plan are not deducted.

## Rules

| Rule | Severity | What it catches |
|------|----------|-----------------|
| `missing-cache`    | WARN | A dependency install (`npm ci`, `go test`, `pip install`, `mvn`, `bundle install`, …) with no cache covering it |
| `unstable-key`     | WARN | An `actions/cache` key with no `hashFiles()` — it never invalidates on dependency change |
| `no-restore-keys`  | INFO | An `actions/cache` step with no `restore-keys` fallback |

## Exit codes

For the linter (the subcommands exit `0` or `2`):

| Code | Meaning |
|------|---------|
| `0`  | No warnings (info-only findings allowed) |
| `1`  | At least one warning — CI gate |
| `2`  | Runtime error (bad path, unreadable file) |

## License

MIT — see [LICENSE](LICENSE).
