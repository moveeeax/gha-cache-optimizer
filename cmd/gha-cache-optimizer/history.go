package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/moveeeax/gha-cache-optimizer/internal/ghapi"
	"github.com/moveeeax/gha-cache-optimizer/internal/history"
)

// now is replaced in tests.
var now = time.Now

// historyFlags are shared by cache-report and cost.
type historyFlags struct {
	repo, since, format, groupBy, workflow, cacheDir string
	maxRuns, workers                                 int
	noDiskCache                                      bool
}

func (h *historyFlags) register(fs *flag.FlagSet, defaultGroupBy string, defaultMaxRuns int, dims []string) {
	fs.StringVar(&h.repo, "repo", os.Getenv("GITHUB_REPOSITORY"), "repository as OWNER/NAME (default $GITHUB_REPOSITORY)")
	fs.StringVar(&h.since, "since", "30d", "window start: 30d, 2w, 12h or YYYY-MM-DD")
	fs.StringVar(&h.format, "format", "table", "output format: table, csv or json")
	fs.StringVar(&h.groupBy, "group-by", defaultGroupBy, "comma list of: "+strings.Join(dims, ", "))
	fs.StringVar(&h.workflow, "workflow", "", "only runs of this workflow file, e.g. ci.yml")
	fs.IntVar(&h.maxRuns, "max-runs", defaultMaxRuns, "only the newest N runs in the window (0 = all)")
	fs.IntVar(&h.workers, "workers", 4, "parallel API requests")
	fs.StringVar(&h.cacheDir, "cache-dir", "", "on-disk cache of completed runs (default: user cache dir)")
	fs.BoolVar(&h.noDiskCache, "no-disk-cache", false, "do not read or write the on-disk cache")
}

// resolve validates the shared flags and builds the API client.
func (h *historyFlags) resolve(dims []string) (*ghapi.Client, time.Time, history.Format, []string, error) {
	if h.repo == "" {
		return nil, time.Time{}, "", nil, fmt.Errorf("--repo OWNER/NAME is required")
	}
	c, err := ghapi.NewClient(h.repo)
	if err != nil {
		return nil, time.Time{}, "", nil, err
	}
	since, err := history.ParseSince(h.since, now())
	if err != nil {
		return nil, time.Time{}, "", nil, err
	}
	f, err := history.ParseFormat(h.format)
	if err != nil {
		return nil, time.Time{}, "", nil, err
	}
	groupBy, err := history.ParseGroupBy(h.groupBy, dims)
	if err != nil {
		return nil, time.Time{}, "", nil, err
	}
	if h.workers < 1 {
		return nil, time.Time{}, "", nil, fmt.Errorf("--workers must be at least 1")
	}
	if !h.noDiskCache {
		dir := h.cacheDir
		if dir == "" {
			if dir, err = ghapi.DefaultCacheDir(c.Owner, c.Repo); err != nil {
				return nil, time.Time{}, "", nil, fmt.Errorf("no user cache dir (pass --cache-dir or --no-disk-cache): %w", err)
			}
		}
		c.Cache = &ghapi.DiskCache{Dir: dir}
	}
	return c, since, f, groupBy, nil
}

func runCacheReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache-report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var h historyFlags
	h.register(fs, "workflow,job,key-prefix", 200, history.CacheGroupDims())
	view := fs.String("view", "summary", "summary (per group), trend (per group per week) or entries (live caches)")
	limitGB := fs.Float64("cache-limit-gb", history.DefaultRepoCacheLimitGB, "repository cache size limit used for the quota line")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: gha-cache-optimizer cache-report --repo OWNER/NAME [flags]\n\n")
		fmt.Fprintf(stderr, "Reads job logs of recent runs and reports cache hit, partial hit and miss\n")
		fmt.Fprintf(stderr, "counts with restore and save times. Token from GITHUB_TOKEN or GH_TOKEN.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	v := history.CacheView(strings.ToLower(*view))
	if v != history.ViewSummary && v != history.ViewTrend && v != history.ViewEntries {
		fmt.Fprintf(stderr, "error: unknown --view %q (want summary, trend or entries)\n", *view)
		return 2
	}
	c, since, f, groupBy, err := h.resolve(history.CacheGroupDims())
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	warnIfNoToken(c, stderr)
	rep, err := history.BuildCacheReport(c, history.CacheOptions{
		Since: since, Until: now(), Workflow: h.workflow, GroupBy: groupBy, View: v,
		MaxRuns: h.maxRuns, LimitGB: *limitGB, Workers: h.workers, Warn: stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if err := rep.Write(stdout, f); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	return 0
}

// rateFlags collects repeated --rate values.
type rateFlags []string

func (r *rateFlags) String() string     { return strings.Join(*r, ",") }
func (r *rateFlags) Set(v string) error { *r = append(*r, v); return nil }

func runCost(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cost", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var h historyFlags
	h.register(fs, "workflow,matrix", 0, history.CostGroupDims())
	var rates rateFlags
	fs.Var(&rates, "rate", "price a runs-on label: LABEL=USD_PER_MIN or LABEL=SKU (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: gha-cache-optimizer cost --repo OWNER/NAME [flags]\n\n")
		fmt.Fprintf(stderr, "Estimates billed runner minutes and list-price cost per workflow, job and\n")
		fmt.Fprintf(stderr, "matrix leg from run history. Token from GITHUB_TOKEN or GH_TOKEN.\n")
		fmt.Fprintf(stderr, "Known SKUs: %s\n\n", strings.Join(history.SKUs(), ", "))
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	pricing, err := history.NewPricing(rates)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	c, since, f, groupBy, err := h.resolve(history.CostGroupDims())
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	warnIfNoToken(c, stderr)
	rep, err := history.BuildCostReport(c, history.CostOptions{
		Since: since, Until: now(), Workflow: h.workflow, GroupBy: groupBy,
		MaxRuns: h.maxRuns, Pricing: pricing, Workers: h.workers,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	if err := rep.Write(stdout, f); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	return 0
}

func warnIfNoToken(c *ghapi.Client, stderr io.Writer) {
	if c.Token == "" {
		fmt.Fprintln(stderr, "warning: no GITHUB_TOKEN or GH_TOKEN; unauthenticated requests are limited to 60 per hour")
	}
}
