// Package analyzer parses GitHub Actions workflow YAML and reports missing or
// ineffective dependency caching, each with a ready-to-paste fix.
package analyzer

// Severity ranks a finding.
type Severity string

const (
	// SevWarn is a real cache miss costing CI time.
	SevWarn Severity = "WARN"
	// SevInfo is a caching config that works but could be sturdier.
	SevInfo Severity = "INFO"
)

// Rule identifies the kind of problem a finding reports.
type Rule string

const (
	// RuleMissingCache: a dependency-installing step runs with no cache at all.
	RuleMissingCache Rule = "missing-cache"
	// RuleUnstableKey: an actions/cache key has no lockfile hash, so it never
	// invalidates and serves stale content (or never updates).
	RuleUnstableKey Rule = "unstable-key"
	// RuleNoRestoreKeys: an actions/cache step has no restore-keys, so a key
	// miss falls back to nothing instead of the newest prior cache.
	RuleNoRestoreKeys Rule = "no-restore-keys"
)

// Finding is one reported problem, anchored to a file/job/step.
type Finding struct {
	File       string   `json:"file"`
	Job        string   `json:"job"`
	Step       string   `json:"step"`
	Line       int      `json:"line"`
	Rule       Rule     `json:"rule"`
	Severity   Severity `json:"severity"`
	Message    string   `json:"message"`
	Suggestion string   `json:"suggestion,omitempty"`
}

// Report is the full analysis result across all scanned files.
type Report struct {
	Findings   []Finding `json:"findings"`
	FilesCount int       `json:"files_scanned"`
	WarnCount  int       `json:"warn_count"`
	InfoCount  int       `json:"info_count"`
}
