package ghapi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GITHUB_TOKEN", "test-token")
	c, err := NewClient("acme/app")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClientRejectsBadRepo(t *testing.T) {
	for _, bad := range []string{"", "acme", "/app", "acme/", "a/b/c"} {
		if _, err := NewClient(bad); err == nil {
			t.Fatalf("NewClient(%q) should fail", bad)
		}
	}
}

func TestListRunsFollowsLinkAndDropsIncomplete(t *testing.T) {
	var base string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Errorf("missing auth or version header: %v", r.Header)
		}
		if r.URL.Path != "/repos/acme/app/actions/workflows/ci.yml/runs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.Query().Get("created"), "..") {
			t.Errorf("created filter missing: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2&created=x..y>; rel="next", <%s/x?page=2>; rel="last"`, base, r.URL.Path, base))
			fmt.Fprint(w, `{"total_count":3,"workflow_runs":[
				{"id":2,"status":"completed","created_at":"2026-10-02T00:00:00Z"},
				{"id":3,"status":"in_progress","created_at":"2026-10-03T00:00:00Z"}]}`)
			return
		}
		fmt.Fprint(w, `{"total_count":3,"workflow_runs":[{"id":1,"status":"completed","created_at":"2026-10-01T00:00:00Z"}]}`)
	}))
	base = c.BaseURL
	runs, err := c.ListRuns(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), "ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != 1 || runs[1].ID != 2 {
		t.Fatalf("want completed runs 1,2 oldest first, got %+v", runs)
	}
}

// A window over the 1,000-result cap is split until each half fits.
func TestListRunsSplitsOverCap(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		from, to, _ := strings.Cut(r.URL.Query().Get("created"), "..")
		f, _ := time.Parse(time.RFC3339, from)
		e, _ := time.Parse(time.RFC3339, to)
		if e.Sub(f) > 24*time.Hour {
			fmt.Fprint(w, `{"total_count":1500,"workflow_runs":[]}`)
			return
		}
		// Every half returns the same boundary run once, to check de-duplication.
		fmt.Fprintf(w, `{"total_count":2,"workflow_runs":[
			{"id":%d,"status":"completed","created_at":%q},
			{"id":99,"status":"completed","created_at":"2026-10-01T12:00:00Z"}]}`, f.Unix(), f.Format(time.RFC3339))
	}))
	runs, err := c.ListRuns(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("want 1 oversized + 2 halves = 3 calls, got %d", calls.Load())
	}
	if len(runs) != 3 {
		t.Fatalf("want 3 unique runs, got %+v", runs)
	}
}

func TestRunJobsUsesDiskCache(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("filter") != "all" {
			t.Errorf("jobs must be listed with filter=all to include re-run attempts")
		}
		fmt.Fprint(w, `{"total_count":1,"jobs":[{"id":7,"name":"build","labels":["ubuntu-latest"],
			"started_at":"2026-10-01T00:00:10Z","completed_at":"2026-10-01T00:01:10Z"}]}`)
	}))
	c.Cache = &DiskCache{Dir: t.TempDir()}
	run := Run{ID: 5, RunAttempt: 1, Status: "completed"}
	for i := 0; i < 2; i++ {
		jobs, err := c.RunJobs(run)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 || jobs[0].ID != 7 || jobs[0].Labels[0] != "ubuntu-latest" {
			t.Fatalf("jobs = %+v", jobs)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("second call should come from disk, got %d API calls", calls.Load())
	}
	// A new attempt of the same run is a different cache entry.
	if _, err := c.RunJobs(Run{ID: 5, RunAttempt: 2, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("attempt 2 should be fetched, got %d calls", calls.Load())
	}
}

// The logs endpoint redirects to a signed URL on another host; the token must
// not follow it.
func TestJobLogFollowsRedirectWithoutToken(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("token leaked to log storage")
		}
		fmt.Fprint(w, "2026-10-01T00:00:00Z hello\n")
	}))
	defer storage.Close()
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/app/actions/jobs/42/logs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		// 127.0.0.1 vs localhost makes the redirect cross-host.
		http.Redirect(w, r, strings.Replace(storage.URL, "127.0.0.1", "localhost", 1)+"/log", http.StatusFound)
	}))
	body, err := c.JobLog(42)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if !strings.Contains(string(data), "hello") {
		t.Fatalf("log body = %q", data)
	}
}

func TestErrorsCarryMessageAndRateLimitReset(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			w.WriteHeader(http.StatusGone)
			fmt.Fprint(w, `{"message":"Gone"}`)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1793000000")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	_, err := c.GetCacheUsage()
	if err == nil || !strings.Contains(err.Error(), "API rate limit exceeded") || !strings.Contains(err.Error(), "resets at") {
		t.Fatalf("err = %v", err)
	}
	if NotFound(err) {
		t.Fatal("403 is not NotFound")
	}
	if _, err := c.JobLog(1); !NotFound(err) {
		t.Fatalf("410 should count as NotFound, got %v", err)
	}
}

func TestListCachesAndUsage(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/app/actions/caches":
			fmt.Fprint(w, `{"total_count":1,"actions_caches":[{"id":1,"ref":"refs/heads/main","key":"Linux-node-abc","size_in_bytes":1024,
				"last_accessed_at":"2026-10-01T00:00:00Z","created_at":"2026-09-01T00:00:00Z"}]}`)
		case "/repos/acme/app/actions/cache/usage":
			fmt.Fprint(w, `{"full_name":"acme/app","active_caches_size_in_bytes":1024,"active_caches_count":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	entries, err := c.ListCaches()
	if err != nil || len(entries) != 1 || entries[0].Key != "Linux-node-abc" || entries[0].SizeInBytes != 1024 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	u, err := c.GetCacheUsage()
	if err != nil || u.ActiveCachesCount != 1 || u.ActiveCachesSizeInBytes != 1024 {
		t.Fatalf("usage = %+v, %v", u, err)
	}
}

func TestForEachReturnsFirstError(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	out := make([]int, len(items))
	if err := ForEach(items, 3, func(i, v int) error { out[i] = v * 2; return nil }); err != nil {
		t.Fatal(err)
	}
	if out[4] != 10 {
		t.Fatalf("out = %v", out)
	}
	err := ForEach(items, 2, func(i, v int) error {
		if v == 3 {
			return fmt.Errorf("boom")
		}
		return nil
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
}

func TestRunWorkflowLabel(t *testing.T) {
	cases := map[Run]string{
		{Path: ".github/workflows/ci.yml"}:              "ci.yml",
		{Path: ".github/workflows/reuse.yml@refs/x"}:    "reuse.yml",
		{Path: "dynamic/dependabot/dependabot-updates"}: "dynamic/dependabot/dependabot-updates",
		{Name: "legacy"}: "legacy",
	}
	for r, want := range cases {
		if got := r.Workflow(); got != want {
			t.Fatalf("Workflow(%+v) = %q, want %q", r, got, want)
		}
	}
}
