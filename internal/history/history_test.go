package history

import (
	"bytes"
	"encoding/csv"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moveeeax/gha-cache-optimizer/internal/ghapi"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// The fixture has one restore of each kind: a setup-go miss whose key only
// appears in the post-step save, a partial actions/cache hit through
// restore-keys, an exact actions/cache hit on a static key, and a setup-node
// exact hit.
func TestParseCacheLogFixture(t *testing.T) {
	f, err := os.Open("testdata/cache-job.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	evs, err := ParseCacheLog(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("want 4 events, got %d: %+v", len(evs), evs)
	}

	goMiss := evs[0]
	if goMiss.Action != "actions/setup-go@v7" || goMiss.Outcome != OutcomeMiss {
		t.Fatalf("event 0 = %+v, want setup-go miss", goMiss)
	}
	if !near(goMiss.RestoreSeconds, 3.5) {
		t.Fatalf("setup-go restore seconds = %v, want 3.5", goMiss.RestoreSeconds)
	}
	if !goMiss.Saved || !near(goMiss.SaveSeconds, 3) {
		t.Fatalf("setup-go save not attributed: %+v", goMiss)
	}
	if got := goMiss.KeyPrefix(); got != "setup-go-Linux-x64-ubuntu24-go-1.27.3-" {
		t.Fatalf("setup-go prefix = %q (key comes from the save line)", got)
	}

	partial := evs[1]
	if partial.Outcome != OutcomePartial {
		t.Fatalf("event 1 outcome = %s, want partial (restore-keys match)", partial.Outcome)
	}
	if !near(partial.RestoreSeconds, 4) || !partial.Saved || !near(partial.SaveSeconds, 4) {
		t.Fatalf("event 1 timings = %+v, want restore 4s, save 4s", partial)
	}
	if got := partial.KeyPrefix(); got != "Linux-node-" {
		t.Fatalf("event 1 prefix = %q, want Linux-node-", got)
	}

	static := evs[2]
	if static.Outcome != OutcomeHit || static.Saved || static.KeyPrefix() != "tools-v3" {
		t.Fatalf("event 2 = %+v, want unsaved exact hit on tools-v3", static)
	}

	node := evs[3]
	if node.Action != "actions/setup-node@v7" || node.Outcome != OutcomeHit || node.Saved {
		t.Fatalf("event 3 = %+v, want setup-node exact hit with no save", node)
	}
	if got := node.KeyPrefix(); got != "node-cache-Linux-x64-npm-" {
		t.Fatalf("setup-node prefix = %q", got)
	}
}

func TestParseCacheLogNotFoundUsesFirstInputKey(t *testing.T) {
	log := "2026-10-06T10:00:00Z ##[group]Run actions/cache/restore@v6\n" +
		"2026-10-06T10:00:00Z ##[endgroup]\n" +
		"2026-10-06T10:00:02Z Cache not found for input keys: Linux-pip-abcdefabcdefabcdef, Linux-pip-\n"
	evs, err := ParseCacheLog(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Outcome != OutcomeMiss || evs[0].KeyPrefix() != "Linux-pip-" || !near(evs[0].RestoreSeconds, 2) {
		t.Fatalf("got %+v", evs)
	}
}

func TestKeyPrefixWithoutKeyUsesAction(t *testing.T) {
	e := CacheEvent{Action: "actions/setup-python@v7", Outcome: OutcomeMiss}
	if got := e.KeyPrefix(); got != "(actions/setup-python)" {
		t.Fatalf("prefix = %q", got)
	}
}

func rec(workflow, job string, when string, o Outcome, key string) CacheRecord {
	ts, _ := time.Parse(time.RFC3339, when)
	return CacheRecord{Workflow: workflow, Job: job, Leg: job, When: ts,
		CacheEvent: CacheEvent{Action: "actions/cache@v6", PrimaryKey: key, Outcome: o, RestoreSeconds: 2}}
}

func TestAggregateCacheSummaryWorstFirst(t *testing.T) {
	records := []CacheRecord{
		rec("ci.yml", "build", "2026-09-01T10:00:00Z", OutcomeHit, "Linux-go-aaaaaaaaaaaaaaaaaaaa"),
		rec("ci.yml", "build", "2026-09-02T10:00:00Z", OutcomeHit, "Linux-go-bbbbbbbbbbbbbbbbbbbb"),
		rec("ci.yml", "lint", "2026-03-02T10:00:00Z", OutcomeHit, "lint-cccccccccccccccccccc"),
		rec("ci.yml", "lint", "2026-09-03T10:00:00Z", OutcomeMiss, "lint-dddddddddddddddddddd"),
		rec("ci.yml", "lint", "2026-09-04T10:00:00Z", OutcomeMiss, "lint-eeeeeeeeeeeeeeeeeeee"),
	}
	rows := AggregateCache(records, []string{"job", "key-prefix"}, false)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %+v", rows)
	}
	lint := rows[0]
	if lint.Group["job"] != "lint" || lint.Group["key-prefix"] != "lint-" {
		t.Fatalf("worst group should come first, got %+v", lint.Group)
	}
	if lint.Restores != 3 || lint.Hits != 1 || lint.Misses != 2 || !near(lint.HitRate, 33.33) {
		t.Fatalf("lint row = %+v", lint)
	}
	if lint.LastHit == nil || lint.LastHit.Format("2006-01-02") != "2026-03-02" {
		t.Fatalf("lint last hit = %v, want 2026-03-02", lint.LastHit)
	}
	if rows[1].HitRate != 100 || !near(rows[1].AvgRestoreSecs, 2) {
		t.Fatalf("build row = %+v", rows[1])
	}
}

func TestAggregateCacheTrendByWeek(t *testing.T) {
	records := []CacheRecord{
		rec("ci.yml", "build", "2026-10-05T10:00:00Z", OutcomeHit, "k-aaaaaaaaaaaaaaaaaaaa"),  // Monday
		rec("ci.yml", "build", "2026-10-11T23:00:00Z", OutcomeHit, "k-aaaaaaaaaaaaaaaaaaaa"),  // Sunday, same week
		rec("ci.yml", "build", "2026-10-12T01:00:00Z", OutcomeMiss, "k-bbbbbbbbbbbbbbbbbbbb"), // next Monday
	}
	rows := AggregateCache(records, []string{"key-prefix"}, true)
	if len(rows) != 2 {
		t.Fatalf("want 2 weekly rows, got %+v", rows)
	}
	if rows[0].Week != "2026-10-05" || rows[0].Hits != 2 || rows[1].Week != "2026-10-12" || rows[1].Misses != 1 {
		t.Fatalf("trend rows = %+v", rows)
	}
}

func TestCacheReportCSVAndText(t *testing.T) {
	rep := &CacheReport{
		View:    ViewSummary,
		GroupBy: []string{"workflow"},
		Rows: AggregateCache([]CacheRecord{
			rec("ci.yml", "build", "2026-10-05T10:00:00Z", OutcomeMiss, "k-"),
		}, []string{"workflow"}, false),
		Usage:   &ghapi.CacheUsage{ActiveCachesSizeInBytes: 2_500_000_000, ActiveCachesCount: 7},
		LimitGB: 10,
	}
	var buf bytes.Buffer
	if err := rep.Write(&buf, FormatCSV); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0][0] != "workflow" || recs[1][0] != "ci.yml" || recs[1][len(recs[1])-1] != "never" {
		t.Fatalf("csv = %v", recs)
	}
	buf.Reset()
	if err := rep.Write(&buf, FormatTable); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "cache storage: 2.50 GB in 7 entries (25.0% of 10 GB limit)") {
		t.Fatalf("text output missing quota line:\n%s", buf.String())
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"30d":        now.AddDate(0, 0, -30),
		"2w":         now.AddDate(0, 0, -14),
		"12h":        now.Add(-12 * time.Hour),
		"2026-03-01": time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Fatalf("ParseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "30", "0d", "3m", "2027-01-01"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Fatalf("ParseSince(%q) should fail", bad)
		}
	}
}

func TestParseGroupBy(t *testing.T) {
	got, err := ParseGroupBy("Workflow, matrix,workflow", CostGroupDims())
	if err != nil || strings.Join(got, ",") != "workflow,matrix" {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := ParseGroupBy("key-prefix", CostGroupDims()); err == nil {
		t.Fatal("key-prefix is not a cost dimension")
	}
}

func TestBaseJobName(t *testing.T) {
	cases := map[string]string{
		"test (1.26, ubuntu-latest)": "test",
		"build":                      "build",
		"deploy (prod) (eu)":         "deploy (prod)",
	}
	for in, want := range cases {
		if got := baseJobName(in); got != want {
			t.Fatalf("baseJobName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPricingResolve(t *testing.T) {
	p, err := NewPricing([]string{"big-linux=linux_8_core", "gpu-box=0.5"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		labels []string
		class  string
		rate   float64
		billed bool
	}{
		{[]string{"ubuntu-latest"}, "actions_linux", 0.006, true},
		{[]string{"ubuntu-24.04-arm"}, "actions_linux_arm", 0.005, true},
		{[]string{"ubuntu-slim"}, "actions_linux_slim", 0.002, true},
		{[]string{"windows-2025"}, "actions_windows", 0.010, true},
		{[]string{"windows-11-arm"}, "actions_windows_arm", 0.010, true},
		{[]string{"macos-15"}, "actions_macos", 0.062, true},
		{[]string{"macos-15-large"}, "macos_l", 0.077, true},
		{[]string{"macos-latest-xlarge"}, "macos_xl", 0.102, true},
		{[]string{"self-hosted", "linux", "x64"}, "self-hosted", 0, false},
		{[]string{"big-linux"}, "big-linux", 0.022, true},
		{[]string{"GPU-Box"}, "GPU-Box", 0.5, true},
		{[]string{"mystery"}, "unpriced:mystery", 0, false},
	}
	for _, c := range cases {
		r := p.Resolve(c.labels)
		if r.Class != c.class || !near(r.Rate, c.rate) || r.Billed != c.billed {
			t.Fatalf("Resolve(%v) = %+v, want %s %v billed=%v", c.labels, r, c.class, c.rate, c.billed)
		}
	}
	for _, bad := range []string{"novalue", "x=", "x=-1", "x=not_a_sku"} {
		if _, err := NewPricing([]string{bad}); err == nil {
			t.Fatalf("NewPricing(%q) should fail", bad)
		}
	}
}

func job(name string, labels []string, created, started, completed string) ghapi.Job {
	p := func(s string) time.Time { ts, _ := time.Parse(time.RFC3339, s); return ts }
	return ghapi.Job{Name: name, Labels: labels, CreatedAt: p(created), StartedAt: p(started), CompletedAt: p(completed)}
}

func TestCostOfRoundsUpAndSplitsQueue(t *testing.T) {
	p, _ := NewPricing(nil)
	run := ghapi.Run{Path: ".github/workflows/ci.yml"}
	r, ok := CostOf(run, job("test (macos)", []string{"macos-15"},
		"2026-10-01T10:00:00Z", "2026-10-01T10:02:00Z", "2026-10-01T10:05:01Z"), p)
	if !ok {
		t.Fatal("job should be priced")
	}
	if r.Workflow != "ci.yml" || r.Job != "test" || r.Leg != "test (macos)" {
		t.Fatalf("labels = %+v", r)
	}
	if !near(r.QueueSeconds, 120) || !near(r.ExecSeconds, 181) {
		t.Fatalf("queue/exec = %v/%v, want 120/181", r.QueueSeconds, r.ExecSeconds)
	}
	if r.BilledMinutes != 4 || !near(r.Cost, 4*0.062) {
		t.Fatalf("billed = %d, cost = %v; want 4 min (3m01s rounded up), $0.248", r.BilledMinutes, r.Cost)
	}

	if _, ok := CostOf(run, ghapi.Job{Name: "skipped"}, p); ok {
		t.Fatal("a job that never started should be dropped")
	}
	sh, _ := CostOf(run, job("x", []string{"self-hosted"}, "2026-10-01T10:00:00Z", "2026-10-01T10:00:00Z", "2026-10-01T10:10:00Z"), p)
	if sh.BilledMinutes != 0 || sh.Cost != 0 || !near(sh.ExecSeconds, 600) {
		t.Fatalf("self-hosted = %+v, want exec time but no billing", sh)
	}
}

func TestAggregateCostMostExpensiveFirst(t *testing.T) {
	p, _ := NewPricing(nil)
	run := ghapi.Run{Path: ".github/workflows/ci.yml"}
	var records []CostRecord
	for _, j := range []ghapi.Job{
		job("test (linux)", []string{"ubuntu-latest"}, "2026-10-01T10:00:00Z", "2026-10-01T10:00:00Z", "2026-10-01T10:10:00Z"),
		job("test (linux)", []string{"ubuntu-latest"}, "2026-10-02T10:00:00Z", "2026-10-02T10:00:00Z", "2026-10-02T10:10:00Z"),
		job("test (macos)", []string{"macos-15"}, "2026-10-01T10:00:00Z", "2026-10-01T10:00:00Z", "2026-10-01T10:05:00Z"),
		job("mystery", []string{"custom-label"}, "2026-10-01T10:00:00Z", "2026-10-01T10:00:00Z", "2026-10-01T10:05:00Z"),
	} {
		r, _ := CostOf(run, j, p)
		records = append(records, r)
	}
	rows, total := AggregateCost(records, []string{"matrix"})
	if len(rows) != 3 || rows[0].Group["matrix"] != "test (macos)" {
		t.Fatalf("macOS leg (5 min x $0.062) should top 20 Linux minutes x $0.006, got %+v", rows)
	}
	if !near(rows[0].CostUSD, 0.31) || !near(rows[1].CostUSD, 0.12) || rows[1].Jobs != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if total.Jobs != 4 || total.BilledMinutes != 25 || !near(total.CostUSD, 0.43) {
		t.Fatalf("total = %+v", total)
	}
	if got := unpriced(records); len(got) != 1 || got[0] != "custom-label" {
		t.Fatalf("unpriced = %v", got)
	}

	rep := &CostReport{GroupBy: []string{"matrix"}, Rows: rows, Total: total}
	var buf bytes.Buffer
	if err := rep.Write(&buf, FormatCSV); err != nil {
		t.Fatal(err)
	}
	recs, _ := csv.NewReader(&buf).ReadAll()
	last := recs[len(recs)-1]
	if last[0] != "TOTAL" || last[4] != "25" || last[5] != "0.430" {
		t.Fatalf("csv total row = %v", last)
	}
}

func TestWeekStart(t *testing.T) {
	cases := map[string]string{
		"2026-11-09T00:00:00Z": "2026-11-09", // Monday
		"2026-11-15T23:59:59Z": "2026-11-09", // Sunday
		"2026-11-16T00:00:00Z": "2026-11-16",
	}
	for in, want := range cases {
		ts, _ := time.Parse(time.RFC3339, in)
		if got := weekStart(ts); got != want {
			t.Fatalf("weekStart(%s) = %s, want %s", in, got, want)
		}
	}
}
