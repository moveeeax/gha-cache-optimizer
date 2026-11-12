package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI serves one completed run of ci.yml with a two-leg matrix (Linux and
// macOS) and the cache log fixture for the Linux leg.
func fakeAPI(t *testing.T) *atomic.Int32 {
	t.Helper()
	logData, err := os.ReadFile("../../internal/history/testdata/cache-job.log")
	if err != nil {
		t.Fatal(err)
	}
	var logCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/app/actions/runs":
			fmt.Fprint(w, `{"total_count":1,"workflow_runs":[{"id":100,"path":".github/workflows/ci.yml","status":"completed",
				"run_attempt":1,"created_at":"2026-10-06T09:59:00Z"}]}`)
		case "/repos/acme/app/actions/runs/100/jobs":
			fmt.Fprint(w, `{"total_count":3,"jobs":[
				{"id":1,"name":"test (ubuntu-latest)","labels":["ubuntu-latest"],"conclusion":"success",
				 "created_at":"2026-10-06T09:59:00Z","started_at":"2026-10-06T10:00:00Z","completed_at":"2026-10-06T10:01:12Z"},
				{"id":2,"name":"test (macos-15)","labels":["macos-15"],"conclusion":"success",
				 "created_at":"2026-10-06T09:59:00Z","started_at":"2026-10-06T10:03:00Z","completed_at":"2026-10-06T10:13:00Z"},
				{"id":3,"name":"deploy","labels":["ubuntu-latest"],"conclusion":"skipped",
				 "created_at":"2026-10-06T09:59:00Z","started_at":null,"completed_at":"2026-10-06T10:13:00Z"}]}`)
		case "/repos/acme/app/actions/jobs/1/logs":
			logCalls.Add(1)
			w.Write(logData)
		case "/repos/acme/app/actions/jobs/2/logs":
			logCalls.Add(1)
			w.WriteHeader(http.StatusGone) // past retention
		case "/repos/acme/app/actions/cache/usage":
			fmt.Fprint(w, `{"active_caches_size_in_bytes":1000000000,"active_caches_count":3}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GITHUB_TOKEN", "test-token")
	now = func() time.Time { return time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = time.Now })
	return &logCalls
}

func TestCacheReportEndToEnd(t *testing.T) {
	logCalls := fakeAPI(t)
	cacheDir := t.TempDir()
	args := []string{"cache-report", "--repo", "acme/app", "--since", "7d", "--group-by", "key-prefix", "--format", "json", "--cache-dir", cacheDir}
	var out, errb bytes.Buffer
	if code := run(args, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	var rep struct {
		RunsScanned     int `json:"runs_scanned"`
		JobsScanned     int `json:"jobs_scanned"`
		JobsWithoutLogs int `json:"jobs_without_logs"`
		Rows            []struct {
			Group   map[string]string `json:"group"`
			Hits    int               `json:"hits"`
			Partial int               `json:"partial"`
			Misses  int               `json:"misses"`
		} `json:"rows"`
		Usage struct {
			Count int `json:"active_caches_count"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out.String())
	}
	if rep.RunsScanned != 1 || rep.JobsScanned != 2 || rep.JobsWithoutLogs != 1 {
		t.Fatalf("counts = %+v (skipped job must not be fetched; 410 log counted)", rep)
	}
	if len(rep.Rows) != 4 || rep.Usage.Count != 3 {
		t.Fatalf("rows = %+v usage = %+v", rep.Rows, rep.Usage)
	}
	// Worst first: the setup-go miss.
	if rep.Rows[0].Group["key-prefix"] != "setup-go-Linux-x64-ubuntu24-go-1.27.3-" || rep.Rows[0].Misses != 1 {
		t.Fatalf("first row = %+v", rep.Rows[0])
	}

	// Second run: the parsed log of job 1 comes from disk; job 2 had no log
	// and is asked again.
	before := logCalls.Load()
	out.Reset()
	if code := run(args, &out, &errb); code != 0 {
		t.Fatalf("exit %d on rerun", code)
	}
	if got := logCalls.Load() - before; got != 1 {
		t.Fatalf("rerun fetched %d logs, want 1 (only the unavailable one)", got)
	}
}

func TestCacheReportTrendTable(t *testing.T) {
	fakeAPI(t)
	var out, errb bytes.Buffer
	code := run([]string{"cache-report", "--repo", "acme/app", "--view", "trend", "--group-by", "workflow", "--no-disk-cache"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"workflow", "week", "ci.yml", "2026-10-05", "1 run(s), 2 job(s) scanned", "cache storage: 1.00 GB in 3 entries (10.0% of 10 GB limit)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
}

func TestCostEndToEnd(t *testing.T) {
	fakeAPI(t)
	var out, errb bytes.Buffer
	code := run([]string{"cost", "--repo", "acme/app", "--group-by", "matrix,runner", "--format", "csv", "--no-disk-cache"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{
		"matrix,runner,jobs,exec_min,queue_min,billed_min,cost_usd",
		"test (macos-15),actions_macos,1,10.0,4.0,10,0.620",
		"test (ubuntu-latest),actions_linux,1,1.2,1.0,2,0.012",
		"TOTAL,,2,11.2,5.0,12,0.632",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("csv:\n%s\nwant:\n%s", out.String(), strings.Join(want, "\n"))
	}
}

func TestHistoryFlagErrors(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "")
	cases := [][]string{
		{"cost"}, // no repo
		{"cost", "--repo", "acme"},
		{"cost", "--repo", "acme/app", "--since", "soon"},
		{"cost", "--repo", "acme/app", "--group-by", "key-prefix"},
		{"cost", "--repo", "acme/app", "--rate", "x=nope"},
		{"cost", "--repo", "acme/app", "--format", "xml"},
		{"cache-report", "--repo", "acme/app", "--view", "pie"},
		{"cache-report", "--repo", "acme/app", "extra"},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Fatalf("%v: exit %d, want 2 (stderr %q)", args, code, errb.String())
		}
	}
}
