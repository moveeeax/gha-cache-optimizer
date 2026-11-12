package history

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/moveeeax/gha-cache-optimizer/internal/ghapi"
)

// costDims are the --group-by dimensions cost accepts. "matrix" is the full
// job name, so each matrix leg gets its own row.
var costDims = []string{"workflow", "job", "matrix", "runner"}

// CostGroupDims returns the dimensions cost accepts.
func CostGroupDims() []string { return append([]string(nil), costDims...) }

// CostOptions configures the cost report.
type CostOptions struct {
	Since, Until time.Time
	Workflow     string
	GroupBy      []string
	MaxRuns      int
	Pricing      *Pricing
	Workers      int
}

// CostRecord is one job attempt with its timings and price.
type CostRecord struct {
	Workflow string
	Job      string
	Leg      string
	Runner   Runner
	// QueueSeconds is created_at -> started_at, ExecSeconds started_at ->
	// completed_at. Only execution is billed.
	QueueSeconds float64
	ExecSeconds  float64
	// BilledMinutes is ExecSeconds rounded up to a whole minute, as GitHub
	// bills each job; zero for self-hosted and unpriced runners.
	BilledMinutes int
	Cost          float64
}

// CostRow is one aggregated group.
type CostRow struct {
	Group         map[string]string `json:"group"`
	Jobs          int               `json:"jobs"`
	ExecMinutes   float64           `json:"exec_minutes"`
	QueueMinutes  float64           `json:"queue_minutes"`
	BilledMinutes int               `json:"billed_minutes"`
	CostUSD       float64           `json:"cost_usd"`
}

// CostReport is the cost result.
type CostReport struct {
	Repo           string    `json:"repo"`
	Since          time.Time `json:"since"`
	Until          time.Time `json:"until"`
	GroupBy        []string  `json:"group_by"`
	RunsScanned    int       `json:"runs_scanned"`
	Rows           []CostRow `json:"rows"`
	Total          CostRow   `json:"total"`
	UnpricedLabels []string  `json:"unpriced_labels,omitempty"`
}

// BuildCostReport lists runs and their jobs (every attempt, since re-runs are
// billed too) and aggregates estimated cost.
func BuildCostReport(c *ghapi.Client, opt CostOptions) (*CostReport, error) {
	runs, err := c.ListRuns(opt.Since, opt.Until, opt.Workflow)
	if err != nil {
		return nil, err
	}
	if opt.MaxRuns > 0 && len(runs) > opt.MaxRuns {
		runs = runs[len(runs)-opt.MaxRuns:]
	}
	perRun := make([][]ghapi.Job, len(runs))
	if err := ghapi.ForEach(runs, opt.Workers, func(i int, r ghapi.Run) error {
		jobs, err := c.RunJobs(r)
		perRun[i] = jobs
		return err
	}); err != nil {
		return nil, err
	}
	var records []CostRecord
	for i, jobs := range perRun {
		for _, j := range jobs {
			if rec, ok := CostOf(runs[i], j, opt.Pricing); ok {
				records = append(records, rec)
			}
		}
	}
	rep := &CostReport{
		Repo: c.FullName(), Since: opt.Since, Until: opt.Until,
		GroupBy: opt.GroupBy, RunsScanned: len(runs),
	}
	rep.Rows, rep.Total = AggregateCost(records, opt.GroupBy)
	rep.UnpricedLabels = unpriced(records)
	return rep, nil
}

// CostOf prices one job. Jobs that never started are dropped.
func CostOf(run ghapi.Run, j ghapi.Job, p *Pricing) (CostRecord, bool) {
	if j.StartedAt.IsZero() || j.CompletedAt.IsZero() {
		return CostRecord{}, false
	}
	exec := math.Max(0, j.CompletedAt.Sub(j.StartedAt).Seconds())
	queue := 0.0
	if !j.CreatedAt.IsZero() {
		queue = math.Max(0, j.StartedAt.Sub(j.CreatedAt).Seconds())
	}
	rn := p.Resolve(j.Labels)
	rec := CostRecord{
		Workflow:     run.Workflow(),
		Job:          baseJobName(j.Name),
		Leg:          j.Name,
		Runner:       rn,
		QueueSeconds: queue,
		ExecSeconds:  exec,
	}
	if rn.Billed {
		rec.BilledMinutes = int(math.Ceil(exec / 60))
		rec.Cost = float64(rec.BilledMinutes) * rn.Rate
	}
	return rec, true
}

func costDimValue(r CostRecord, dim string) string {
	switch dim {
	case "workflow":
		return r.Workflow
	case "job":
		return r.Job
	case "matrix":
		return r.Leg
	case "runner":
		return r.Runner.Class
	}
	return ""
}

// AggregateCost groups records, most expensive first, and returns the total.
func AggregateCost(records []CostRecord, dims []string) ([]CostRow, CostRow) {
	byKey := map[string]*CostRow{}
	var order []string
	total := CostRow{Group: map[string]string{}}
	for _, r := range records {
		group := map[string]string{}
		parts := make([]string, len(dims))
		for i, d := range dims {
			group[d] = costDimValue(r, d)
			parts[i] = group[d]
		}
		k := strings.Join(parts, "\x00")
		row := byKey[k]
		if row == nil {
			row = &CostRow{Group: group}
			byKey[k] = row
			order = append(order, k)
		}
		for _, x := range []*CostRow{row, &total} {
			x.Jobs++
			x.ExecMinutes += r.ExecSeconds / 60
			x.QueueMinutes += r.QueueSeconds / 60
			x.BilledMinutes += r.BilledMinutes
			x.CostUSD += r.Cost
		}
	}
	rows := make([]CostRow, 0, len(order))
	for _, k := range order {
		rows = append(rows, *byKey[k])
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD > rows[j].CostUSD
		}
		if rows[i].BilledMinutes != rows[j].BilledMinutes {
			return rows[i].BilledMinutes > rows[j].BilledMinutes
		}
		return rows[i].ExecMinutes > rows[j].ExecMinutes
	})
	return rows, total
}

func unpriced(records []CostRecord) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range records {
		if l, ok := strings.CutPrefix(r.Runner.Class, "unpriced:"); ok && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// Write renders the report in the chosen format. Table and CSV end with a
// TOTAL row.
func (r *CostReport) Write(w io.Writer, f Format) error {
	if f == FormatJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	t := table{header: append(append([]string(nil), r.GroupBy...),
		"jobs", "exec_min", "queue_min", "billed_min", "cost_usd")}
	add := func(row CostRow, label bool) {
		var cells []string
		for i, d := range r.GroupBy {
			v := row.Group[d]
			if label {
				v = ""
				if i == 0 {
					v = "TOTAL"
				}
			}
			cells = append(cells, v)
		}
		cells = append(cells, fmt.Sprint(row.Jobs), fmtFloat(row.ExecMinutes, 1),
			fmtFloat(row.QueueMinutes, 1), fmt.Sprint(row.BilledMinutes), fmtFloat(row.CostUSD, 3))
		t.rows = append(t.rows, cells)
	}
	for _, row := range r.Rows {
		add(row, false)
	}
	add(r.Total, true)
	if f == FormatCSV {
		return t.writeCSV(w)
	}
	if err := t.writeText(w); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%d run(s) since %s. Cost is the GitHub-hosted list price per billed minute;\n",
		r.RunsScanned, r.Since.UTC().Format("2006-01-02"))
	fmt.Fprintln(w, "public repositories and included plan minutes are not deducted. Queue time is not billed.")
	if len(r.UnpricedLabels) > 0 {
		fmt.Fprintf(w, "unpriced runner labels (add --rate LABEL=USD or LABEL=SKU): %s\n", strings.Join(r.UnpricedLabels, "; "))
	}
	return nil
}
