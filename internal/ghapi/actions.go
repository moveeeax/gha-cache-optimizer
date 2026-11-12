package ghapi

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Run is the subset of a workflow run the reports use.
type Run struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Event      string    `json:"event"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	RunAttempt int       `json:"run_attempt"`
	HeadBranch string    `json:"head_branch"`
	CreatedAt  time.Time `json:"created_at"`
}

// Workflow returns a stable label for the run's workflow: the file name for
// .github/workflows/*.yml, otherwise the raw path (e.g. dynamic Dependabot runs).
func (r Run) Workflow() string {
	if p, ok := strings.CutPrefix(r.Path, ".github/workflows/"); ok {
		// Reusable-workflow paths can carry an @ref suffix.
		p, _, _ = strings.Cut(p, "@")
		return p
	}
	if r.Path != "" {
		return r.Path
	}
	return r.Name
}

// Job is the subset of a workflow job the reports use.
type Job struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	RunAttempt  int       `json:"run_attempt"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	Labels      []string  `json:"labels"`
	RunnerName  string    `json:"runner_name"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// listRunsCap is the documented ceiling on results the runs endpoint returns
// for a query that uses the `created` filter.
const listRunsCap = 1000

// ListRuns returns completed runs created in [from, to], optionally limited to
// one workflow file (e.g. "ci.yml"). A window whose result count hits the
// API's 1,000-result cap is split in half until each piece fits.
func (c *Client) ListRuns(from, to time.Time, workflow string) ([]Run, error) {
	seen := map[int64]bool{}
	var out []Run
	var walk func(from, to time.Time) error
	walk = func(from, to time.Time) error {
		runs, total, err := c.listRunsWindow(from, to, workflow)
		if err != nil {
			return err
		}
		if total > listRunsCap && to.Sub(from) > time.Minute {
			mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
			if err := walk(from, mid); err != nil {
				return err
			}
			return walk(mid, to)
		}
		for _, r := range runs {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
		return nil
	}
	if err := walk(from.UTC().Truncate(time.Second), to.UTC().Truncate(time.Second)); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// listRunsWindow walks every page for one created range. It stops after the
// first page when the total exceeds the cap, since ListRuns will split.
func (c *Client) listRunsWindow(from, to time.Time, workflow string) ([]Run, int, error) {
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("created", from.Format(time.RFC3339)+".."+to.Format(time.RFC3339))
	path := c.repoPath("/actions/runs")
	if workflow != "" {
		path = c.repoPath("/actions/workflows/" + url.PathEscape(workflow) + "/runs")
	}
	next := path + "?" + q.Encode()
	var runs []Run
	total := 0
	for next != "" {
		var page struct {
			TotalCount   int   `json:"total_count"`
			WorkflowRuns []Run `json:"workflow_runs"`
		}
		var err error
		next, err = c.getJSON(next, &page)
		if err != nil {
			return nil, 0, err
		}
		total = page.TotalCount
		if total > listRunsCap && to.Sub(from) > time.Minute {
			return nil, total, nil
		}
		for _, r := range page.WorkflowRuns {
			if r.Status == "completed" {
				runs = append(runs, r)
			}
		}
	}
	return runs, total, nil
}

// RunJobs returns the jobs of every attempt of a completed run. Results are
// served from and written to the disk cache, keyed by run id and attempt.
func (c *Client) RunJobs(run Run) ([]Job, error) {
	key := fmt.Sprintf("jobs-%d-%d", run.ID, run.RunAttempt)
	var jobs []Job
	if c.Cache.Load(key, &jobs) {
		return jobs, nil
	}
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("filter", "all")
	next := c.repoPath(fmt.Sprintf("/actions/runs/%d/jobs", run.ID)) + "?" + q.Encode()
	for next != "" {
		var page struct {
			Jobs []Job `json:"jobs"`
		}
		var err error
		next, err = c.getJSON(next, &page)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, page.Jobs...)
	}
	if run.Status == "completed" {
		c.Cache.Store(key, jobs)
	}
	return jobs, nil
}

// JobLog opens the plain-text log of one job. The API answers with a redirect
// to a short-lived download URL, which net/http follows.
func (c *Client) JobLog(jobID int64) (io.ReadCloser, error) {
	resp, err := c.get(c.repoPath(fmt.Sprintf("/actions/jobs/%d/logs", jobID)))
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// CacheEntry is one live entry from the repository cache list.
type CacheEntry struct {
	ID             int64     `json:"id"`
	Ref            string    `json:"ref"`
	Key            string    `json:"key"`
	SizeInBytes    int64     `json:"size_in_bytes"`
	CreatedAt      time.Time `json:"created_at"`
	LastAccessedAt time.Time `json:"last_accessed_at"`
}

// ListCaches returns every live cache entry, most recently used first.
func (c *Client) ListCaches() ([]CacheEntry, error) {
	next := c.repoPath("/actions/caches") + "?per_page=100&sort=last_accessed_at&direction=desc"
	var out []CacheEntry
	for next != "" {
		var page struct {
			ActionsCaches []CacheEntry `json:"actions_caches"`
		}
		var err error
		next, err = c.getJSON(next, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.ActionsCaches...)
	}
	return out, nil
}

// CacheUsage is the repository's active cache footprint.
type CacheUsage struct {
	ActiveCachesSizeInBytes int64 `json:"active_caches_size_in_bytes"`
	ActiveCachesCount       int   `json:"active_caches_count"`
}

// GetCacheUsage returns the repository's active cache size and count.
func (c *Client) GetCacheUsage() (CacheUsage, error) {
	var u CacheUsage
	_, err := c.getJSON(c.repoPath("/actions/cache/usage"), &u)
	return u, err
}

// ForEach runs fn over items with a fixed number of workers and returns the
// first error. Results are written by fn itself (index-addressed), so the
// output order never depends on scheduling.
func ForEach[T any](items []T, workers int, fn func(i int, item T) error) error {
	if workers < 1 {
		workers = 1
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		first   error
		indices = make(chan int)
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indices {
				if err := fn(i, items[i]); err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := range items {
		mu.Lock()
		stop := first != nil
		mu.Unlock()
		if stop {
			break
		}
		indices <- i
	}
	close(indices)
	wg.Wait()
	return first
}
