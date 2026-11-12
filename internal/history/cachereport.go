package history

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/moveeeax/gha-cache-optimizer/internal/ghapi"
)

// CacheView selects which table cache-report prints.
type CacheView string

const (
	// ViewSummary: one row per group with hit rate and restore/save times.
	ViewSummary CacheView = "summary"
	// ViewTrend: one row per group per ISO week.
	ViewTrend CacheView = "trend"
	// ViewEntries: live cache entries from the cache API (no run history).
	ViewEntries CacheView = "entries"
)

// cacheDims are the --group-by dimensions cache-report accepts.
var cacheDims = []string{"workflow", "job", "key-prefix"}

// DefaultRepoCacheLimitGB is GitHub's default per-repository cache size
// limit; entries beyond it are evicted least-recently-used first.
const DefaultRepoCacheLimitGB = 10

// CacheOptions configures cache-report.
type CacheOptions struct {
	Since, Until time.Time
	Workflow     string   // optional workflow file filter, e.g. "ci.yml"
	GroupBy      []string // subset of cacheDims, in column order
	View         CacheView
	MaxRuns      int // newest N runs only; 0 = no limit
	LimitGB      float64
	Workers      int
	Warn         io.Writer // progress and non-fatal problems
}

// CacheRecord is one restore event placed in its run and job.
type CacheRecord struct {
	Workflow string    `json:"workflow"`
	Job      string    `json:"job"`
	Leg      string    `json:"leg"`
	RunID    int64     `json:"run_id"`
	When     time.Time `json:"when"`
	CacheEvent
}

// CacheRow is one aggregated group (summary view) or group-week (trend view).
type CacheRow struct {
	Group          map[string]string `json:"group"`
	Week           string            `json:"week,omitempty"`
	Restores       int               `json:"restores"`
	Hits           int               `json:"hits"`
	Partial        int               `json:"partial"`
	Misses         int               `json:"misses"`
	HitRate        float64           `json:"hit_rate_pct"`
	AvgRestoreSecs float64           `json:"avg_restore_seconds"`
	Saves          int               `json:"saves"`
	AvgSaveSecs    float64           `json:"avg_save_seconds"`
	LastHit        *time.Time        `json:"last_hit,omitempty"`

	restoreSum, saveSum float64
}

// CacheReport is the cache-report result.
type CacheReport struct {
	Repo            string             `json:"repo"`
	Since           time.Time          `json:"since"`
	Until           time.Time          `json:"until"`
	View            CacheView          `json:"view"`
	GroupBy         []string           `json:"group_by,omitempty"`
	RunsScanned     int                `json:"runs_scanned"`
	JobsScanned     int                `json:"jobs_scanned"`
	JobsWithoutLogs int                `json:"jobs_without_logs"`
	Rows            []CacheRow         `json:"rows,omitempty"`
	Usage           *ghapi.CacheUsage  `json:"usage,omitempty"`
	LimitGB         float64            `json:"limit_gb"`
	Entries         []ghapi.CacheEntry `json:"entries,omitempty"`
}

// ParseGroupBy validates a comma-separated --group-by list against allowed.
func ParseGroupBy(s string, allowed []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		d := strings.TrimSpace(strings.ToLower(part))
		if d == "" {
			continue
		}
		ok := false
		for _, a := range allowed {
			if d == a {
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("unknown --group-by %q (want a comma list of %s)", d, strings.Join(allowed, ", "))
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--group-by is empty")
	}
	return out, nil
}

// CacheGroupDims returns the dimensions cache-report accepts.
func CacheGroupDims() []string { return append([]string(nil), cacheDims...) }

// BuildCacheReport fetches what the chosen view needs and aggregates it.
func BuildCacheReport(c *ghapi.Client, opt CacheOptions) (*CacheReport, error) {
	rep := &CacheReport{Repo: c.FullName(), Since: opt.Since, Until: opt.Until, View: opt.View, LimitGB: opt.LimitGB}

	if u, err := c.GetCacheUsage(); err == nil {
		rep.Usage = &u
	} else if opt.Warn != nil {
		fmt.Fprintf(opt.Warn, "warning: cache usage unavailable: %v\n", err)
	}
	if opt.View == ViewEntries {
		entries, err := c.ListCaches()
		if err != nil {
			return nil, err
		}
		rep.Entries = entries
		return rep, nil
	}

	rep.GroupBy = opt.GroupBy
	records, err := collectCacheRecords(c, opt, rep)
	if err != nil {
		return nil, err
	}
	rep.Rows = AggregateCache(records, opt.GroupBy, opt.View == ViewTrend)
	return rep, nil
}

// collectCacheRecords lists runs, their jobs, and the cache events in each
// job's log. Parsed events of completed jobs are kept in the disk cache.
func collectCacheRecords(c *ghapi.Client, opt CacheOptions, rep *CacheReport) ([]CacheRecord, error) {
	runs, err := c.ListRuns(opt.Since, opt.Until, opt.Workflow)
	if err != nil {
		return nil, err
	}
	if opt.MaxRuns > 0 && len(runs) > opt.MaxRuns {
		runs = runs[len(runs)-opt.MaxRuns:] // runs are oldest first
	}
	rep.RunsScanned = len(runs)

	type jobRef struct {
		run ghapi.Run
		job ghapi.Job
	}
	perRun := make([][]ghapi.Job, len(runs))
	if err := ghapi.ForEach(runs, opt.Workers, func(i int, r ghapi.Run) error {
		jobs, err := c.RunJobs(r)
		perRun[i] = jobs
		return err
	}); err != nil {
		return nil, err
	}
	var refs []jobRef
	for i, jobs := range perRun {
		for _, j := range jobs {
			if j.StartedAt.IsZero() || j.Conclusion == "skipped" {
				continue // never ran, so there is no log
			}
			refs = append(refs, jobRef{runs[i], j})
		}
	}
	rep.JobsScanned = len(refs)

	perJob := make([][]CacheEvent, len(refs))
	missing := make([]bool, len(refs))
	if err := ghapi.ForEach(refs, opt.Workers, func(i int, ref jobRef) error {
		key := fmt.Sprintf("cachelog-%d", ref.job.ID)
		var evs []CacheEvent
		if c.Cache.Load(key, &evs) {
			perJob[i] = evs
			return nil
		}
		body, err := c.JobLog(ref.job.ID)
		if ghapi.NotFound(err) {
			missing[i] = true // past log retention, or deleted
			return nil
		}
		if err != nil {
			return err
		}
		defer body.Close()
		evs, err = ParseCacheLog(body)
		if err != nil {
			return fmt.Errorf("job %d log: %w", ref.job.ID, err)
		}
		if evs == nil {
			evs = []CacheEvent{}
		}
		c.Cache.Store(key, evs)
		perJob[i] = evs
		return nil
	}); err != nil {
		return nil, err
	}

	var out []CacheRecord
	for i, ref := range refs {
		if missing[i] {
			rep.JobsWithoutLogs++
			continue
		}
		for _, e := range perJob[i] {
			out = append(out, CacheRecord{
				Workflow:   ref.run.Workflow(),
				Job:        baseJobName(ref.job.Name),
				Leg:        ref.job.Name,
				RunID:      ref.run.ID,
				When:       ref.job.StartedAt,
				CacheEvent: e,
			})
		}
	}
	return out, nil
}

func cacheDimValue(r CacheRecord, dim string) string {
	switch dim {
	case "workflow":
		return r.Workflow
	case "job":
		return r.Job
	case "key-prefix":
		return r.KeyPrefix()
	}
	return ""
}

// AggregateCache groups records by the given dimensions (and by ISO week
// when trend is set). Summary rows come worst first: lowest hit rate, then
// most misses, then most restores; trend rows are ordered by group, then week.
func AggregateCache(records []CacheRecord, dims []string, trend bool) []CacheRow {
	byKey := map[string]*CacheRow{}
	var order []string
	for _, r := range records {
		group := map[string]string{}
		parts := make([]string, 0, len(dims)+1)
		for _, d := range dims {
			v := cacheDimValue(r, d)
			group[d] = v
			parts = append(parts, v)
		}
		week := ""
		if trend {
			week = weekStart(r.When)
			parts = append(parts, week)
		}
		k := strings.Join(parts, "\x00")
		row := byKey[k]
		if row == nil {
			row = &CacheRow{Group: group, Week: week}
			byKey[k] = row
			order = append(order, k)
		}
		row.Restores++
		row.restoreSum += r.RestoreSeconds
		switch r.Outcome {
		case OutcomeHit:
			row.Hits++
			if row.LastHit == nil || r.When.After(*row.LastHit) {
				t := r.When
				row.LastHit = &t
			}
		case OutcomePartial:
			row.Partial++
		case OutcomeMiss:
			row.Misses++
		}
		if r.Saved {
			row.Saves++
			row.saveSum += r.SaveSeconds
		}
	}

	rows := make([]CacheRow, 0, len(order))
	for _, k := range order {
		row := byKey[k]
		row.HitRate = 100 * float64(row.Hits) / float64(row.Restores)
		row.AvgRestoreSecs = row.restoreSum / float64(row.Restores)
		if row.Saves > 0 {
			row.AvgSaveSecs = row.saveSum / float64(row.Saves)
		}
		rows = append(rows, *row)
	}
	groupKey := func(r CacheRow) string {
		parts := make([]string, len(dims))
		for i, d := range dims {
			parts[i] = r.Group[d]
		}
		return strings.Join(parts, "\x00")
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if trend {
			if ga, gb := groupKey(a), groupKey(b); ga != gb {
				return ga < gb
			}
			return a.Week < b.Week
		}
		if a.HitRate != b.HitRate {
			return a.HitRate < b.HitRate
		}
		if a.Misses != b.Misses {
			return a.Misses > b.Misses // a cold start costs more than a partial hit
		}
		if a.Restores != b.Restores {
			return a.Restores > b.Restores
		}
		return groupKey(a) < groupKey(b)
	})
	return rows
}

// Write renders the report in the chosen format.
func (r *CacheReport) Write(w io.Writer, f Format) error {
	if f == FormatJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	t := r.table()
	if f == FormatCSV {
		return t.writeCSV(w)
	}
	if err := t.writeText(w); err != nil {
		return err
	}
	fmt.Fprintln(w)
	if r.View != ViewEntries {
		fmt.Fprintf(w, "%d run(s), %d job(s) scanned since %s", r.RunsScanned, r.JobsScanned, r.Since.UTC().Format("2006-01-02"))
		if r.JobsWithoutLogs > 0 {
			fmt.Fprintf(w, "; %d job log(s) unavailable (expired or deleted)", r.JobsWithoutLogs)
		}
		fmt.Fprintln(w)
	}
	if r.Usage != nil {
		used := float64(r.Usage.ActiveCachesSizeInBytes) / 1e9
		fmt.Fprintf(w, "cache storage: %s GB in %d entries", fmtFloat(used, 2), r.Usage.ActiveCachesCount)
		if r.LimitGB > 0 {
			fmt.Fprintf(w, " (%s%% of %s GB limit)", fmtFloat(100*used/r.LimitGB, 1), fmtFloat(r.LimitGB, 0))
		}
		fmt.Fprintln(w)
	}
	return nil
}

func (r *CacheReport) table() table {
	if r.View == ViewEntries {
		t := table{header: []string{"key", "ref", "size_mb", "last_accessed", "created"}}
		for _, e := range r.Entries {
			t.rows = append(t.rows, []string{
				e.Key, e.Ref, fmtFloat(float64(e.SizeInBytes)/1e6, 1),
				e.LastAccessedAt.UTC().Format(time.RFC3339), e.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
		return t
	}
	header := append([]string(nil), r.GroupBy...)
	if r.View == ViewTrend {
		header = append(header, "week")
	}
	header = append(header, "restores", "hits", "partial", "misses", "hit_rate_pct", "avg_restore_s", "saves", "avg_save_s")
	if r.View == ViewSummary {
		header = append(header, "last_hit")
	}
	t := table{header: header}
	for _, row := range r.Rows {
		var cells []string
		for _, d := range r.GroupBy {
			cells = append(cells, row.Group[d])
		}
		if r.View == ViewTrend {
			cells = append(cells, row.Week)
		}
		cells = append(cells,
			fmt.Sprint(row.Restores), fmt.Sprint(row.Hits), fmt.Sprint(row.Partial), fmt.Sprint(row.Misses),
			fmtPct(row.Hits, row.Restores), fmtFloat(row.AvgRestoreSecs, 1),
			fmt.Sprint(row.Saves), fmtFloat(row.AvgSaveSecs, 1))
		if r.View == ViewSummary {
			last := "never"
			if row.LastHit != nil {
				last = row.LastHit.UTC().Format("2006-01-02")
			}
			cells = append(cells, last)
		}
		t.rows = append(t.rows, cells)
	}
	return t
}
