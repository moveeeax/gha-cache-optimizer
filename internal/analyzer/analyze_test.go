package analyzer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func analyze(t *testing.T, yml string) *Report {
	t.Helper()
	rep := &Report{}
	rep.Findings = analyzeFile("ci.yml", []byte(yml))
	rep.FilesCount = 1
	for _, f := range rep.Findings {
		switch f.Severity {
		case SevWarn:
			rep.WarnCount++
		case SevInfo:
			rep.InfoCount++
		}
	}
	return rep
}

// The Notion acceptance criterion, verbatim: a workflow with `npm ci` and no
// cache reports one finding with a correct actions/cache snippet keyed on
// package-lock.json.
func TestAcceptanceNpmCiNoCache(t *testing.T) {
	yml := `
name: ci
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-node@v7
        with:
          node-version: 20
      - run: npm ci
`
	rep := analyze(t, yml)
	if len(rep.Findings) != 1 {
		t.Fatalf("want exactly 1 finding, got %d: %+v", len(rep.Findings), rep.Findings)
	}
	f := rep.Findings[0]
	if f.Rule != RuleMissingCache {
		t.Fatalf("rule = %s, want missing-cache", f.Rule)
	}
	if f.Severity != SevWarn {
		t.Fatalf("severity = %s, want WARN", f.Severity)
	}
	if !strings.Contains(f.Suggestion, "package-lock.json") {
		t.Fatalf("suggestion missing package-lock.json key:\n%s", f.Suggestion)
	}
	if !strings.Contains(f.Suggestion, "actions/cache@v6") {
		t.Fatalf("suggestion missing actions/cache snippet:\n%s", f.Suggestion)
	}
	if !strings.Contains(f.Suggestion, "actions/setup-node@v7") {
		t.Fatalf("suggestion missing current setup-node major:\n%s", f.Suggestion)
	}
}

func TestSetupNodeWithCacheIsClean(t *testing.T) {
	yml := `
jobs:
  build:
    steps:
      - uses: actions/setup-node@v7
        with:
          cache: npm
      - run: npm ci
`
	rep := analyze(t, yml)
	if len(rep.Findings) != 0 {
		t.Fatalf("setup-node cache:npm should suppress the finding, got %+v", rep.Findings)
	}
}

func TestExplicitCacheStepIsClean(t *testing.T) {
	yml := `
jobs:
  build:
    steps:
      - uses: actions/cache@v6
        with:
          path: ~/.npm
          key: ${{ runner.os }}-npm-${{ hashFiles('**/package-lock.json') }}
          restore-keys: |
            ${{ runner.os }}-npm-
      - run: npm ci
`
	rep := analyze(t, yml)
	if rep.WarnCount != 0 {
		t.Fatalf("well-formed cache should have no warnings, got %+v", rep.Findings)
	}
}

func TestUnstableKeyAndNoRestoreKeys(t *testing.T) {
	yml := `
jobs:
  build:
    steps:
      - uses: actions/cache@v6
        with:
          path: ~/.npm
          key: ${{ runner.os }}-npm-static
      - run: npm ci
`
	rep := analyze(t, yml)
	var haveUnstable, haveNoRestore bool
	for _, f := range rep.Findings {
		switch f.Rule {
		case RuleUnstableKey:
			haveUnstable = true
			if f.Severity != SevWarn {
				t.Fatalf("unstable-key should be WARN")
			}
		case RuleNoRestoreKeys:
			haveNoRestore = true
			if f.Severity != SevInfo {
				t.Fatalf("no-restore-keys should be INFO")
			}
		}
	}
	if !haveUnstable || !haveNoRestore {
		t.Fatalf("want both unstable-key(WARN) and no-restore-keys(INFO), got %+v", rep.Findings)
	}
}

func TestGoDefaultCacheIsClean(t *testing.T) {
	// setup-go caches by default; a plain `go build` after it needs no warning.
	yml := `
jobs:
  build:
    steps:
      - uses: actions/setup-go@v7
        with:
          go-version: "1.22"
      - run: go build ./...
`
	rep := analyze(t, yml)
	if len(rep.Findings) != 0 {
		t.Fatalf("setup-go default cache should suppress, got %+v", rep.Findings)
	}
}

func TestGoWithoutSetupWarns(t *testing.T) {
	yml := `
jobs:
  build:
    steps:
      - run: go test ./...
`
	rep := analyze(t, yml)
	if rep.WarnCount != 1 || rep.Findings[0].Line == 0 {
		t.Fatalf("bare go test should warn once with a line, got %+v", rep.Findings)
	}
	if !strings.Contains(rep.Findings[0].Suggestion, "go.sum") {
		t.Fatalf("go suggestion should key on go.sum:\n%s", rep.Findings[0].Suggestion)
	}
}

func TestPythonPipWarns(t *testing.T) {
	yml := `
jobs:
  build:
    steps:
      - uses: actions/setup-python@v7
      - run: pip install -r requirements.txt
`
	rep := analyze(t, yml)
	if rep.WarnCount != 1 {
		t.Fatalf("pip install without cache should warn once, got %+v", rep.Findings)
	}
	if !strings.Contains(rep.Findings[0].Suggestion, "requirements.txt") {
		t.Fatalf("python suggestion should mention requirements.txt")
	}
}

func TestNonWorkflowYAMLIgnored(t *testing.T) {
	rep := analyze(t, "foo: bar\nbaz: [1,2,3]\n")
	if len(rep.Findings) != 0 {
		t.Fatalf("non-workflow YAML should yield no findings, got %+v", rep.Findings)
	}
}

func TestMalformedYAMLIgnored(t *testing.T) {
	rep := analyze(t, "jobs: [this is: not valid: mapping")
	if len(rep.Findings) != 0 {
		t.Fatalf("unparseable YAML should be skipped, got %+v", rep.Findings)
	}
}

func TestLineNumbersReported(t *testing.T) {
	yml := "jobs:\n  build:\n    steps:\n      - run: npm ci\n"
	rep := analyze(t, yml)
	if len(rep.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(rep.Findings))
	}
	if rep.Findings[0].Line != 4 {
		t.Fatalf("expected finding on line 4 (the run:), got %d", rep.Findings[0].Line)
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	yml := "jobs:\n  b:\n    steps:\n      - run: npm ci\n"
	rep := analyze(t, yml)
	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if back.WarnCount != 1 || len(back.Findings) != 1 {
		t.Fatalf("round trip lost data: %+v", back)
	}
}

func TestReportText(t *testing.T) {
	yml := "jobs:\n  b:\n    steps:\n      - run: npm ci\n"
	rep := analyze(t, yml)
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "missing-cache") || !strings.Contains(out, "1 warning") {
		t.Fatalf("text output missing expected content:\n%s", out)
	}
}
