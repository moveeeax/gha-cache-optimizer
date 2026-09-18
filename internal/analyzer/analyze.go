package analyzer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ScanDir analyzes every *.yml / *.yaml file directly under dir (the usual
// .github/workflows). Files that don't parse as a workflow are skipped, not
// fatal — a repo often has unrelated YAML nearby.
func ScanDir(dir string) (*Report, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(e.Name())); ext == ".yml" || ext == ".yaml" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return ScanFiles(files)
}

// ScanFiles analyzes an explicit list of workflow files.
func ScanFiles(files []string) (*Report, error) {
	rep := &Report{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		fs := analyzeFile(filepath.Base(f), data)
		rep.Findings = append(rep.Findings, fs...)
		rep.FilesCount++
	}
	// Stable ordering: by file, then line.
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		if rep.Findings[i].File != rep.Findings[j].File {
			return rep.Findings[i].File < rep.Findings[j].File
		}
		return rep.Findings[i].Line < rep.Findings[j].Line
	})
	for _, fnd := range rep.Findings {
		switch fnd.Severity {
		case SevWarn:
			rep.WarnCount++
		case SevInfo:
			rep.InfoCount++
		}
	}
	return rep, nil
}

// analyzeFile parses one workflow document and applies the rules. A parse
// failure yields no findings (the file is simply not a workflow we understand).
func analyzeFile(name string, data []byte) []Finding {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	if len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	jobs := mapValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil // not a workflow
	}

	var findings []Finding
	// jobs is a mapping of jobName -> jobNode.
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		jobName := jobs.Content[i].Value
		jobNode := jobs.Content[i+1]
		findings = append(findings, analyzeJob(name, jobName, jobNode)...)
	}
	return findings
}

// analyzeJob walks the steps of one job.
func analyzeJob(file, job string, jobNode *yaml.Node) []Finding {
	steps := mapValue(jobNode, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}

	// First pass: does the job already have an explicit actions/cache step, and
	// does any setup-* declare a built-in cache? Both mean deps are covered.
	hasExplicitCache := false
	setupCacheFor := map[string]bool{} // ecosystem name -> covered by setup-*
	var findings []Finding

	for _, step := range steps.Content {
		uses := mapValue(step, "uses")
		if uses == nil {
			continue
		}
		if usesAction(uses.Value, "actions/cache") {
			hasExplicitCache = true
			findings = append(findings, checkCacheStep(file, job, step)...)
		}
		for _, e := range ecosystems {
			if e.setupAction != "" && usesAction(uses.Value, e.setupAction) {
				with := mapValue(step, "with")
				if with != nil && mapValue(with, "cache") != nil {
					setupCacheFor[e.name] = true
				}
				// setup-go caches by default unless cache: false.
				if e.name == "go" {
					if with == nil || !isFalse(mapValue(with, "cache")) {
						setupCacheFor["go"] = true
					}
				}
			}
		}
	}

	// Second pass: find install commands with no covering cache.
	for _, step := range steps.Content {
		run := mapValue(step, "run")
		if run == nil {
			continue
		}
		e, ok := detectEcosystem(run.Value)
		if !ok {
			continue
		}
		if setupCacheFor[e.name] || hasExplicitCache {
			continue
		}
		findings = append(findings, Finding{
			File:       file,
			Job:        job,
			Step:       stepName(step, run.Value),
			Line:       run.Line,
			Rule:       RuleMissingCache,
			Severity:   SevWarn,
			Message:    fmt.Sprintf("%s dependency install with no cache — every run re-downloads from scratch", e.name),
			Suggestion: cacheSnippet(e),
		})
	}
	return findings
}

// checkCacheStep validates an existing actions/cache step (key stability,
// restore-keys presence).
func checkCacheStep(file, job string, step *yaml.Node) []Finding {
	with := mapValue(step, "with")
	if with == nil {
		return nil
	}
	var out []Finding
	name := stepName(step, "actions/cache")

	if key := mapValue(with, "key"); key != nil {
		if !strings.Contains(key.Value, "hashFiles(") {
			out = append(out, Finding{
				File:     file,
				Job:      job,
				Step:     name,
				Line:     key.Line,
				Rule:     RuleUnstableKey,
				Severity: SevWarn,
				Message:  "cache key has no hashFiles() component — it never invalidates when dependencies change",
				Suggestion: "Include a lockfile hash so the key changes with your deps, e.g.\n" +
					"    key: ${{ runner.os }}-deps-${{ hashFiles('**/package-lock.json') }}",
			})
		}
	}
	if rk := mapValue(with, "restore-keys"); rk == nil {
		line := with.Line
		if k := mapValue(with, "key"); k != nil {
			line = k.Line
		}
		out = append(out, Finding{
			File:     file,
			Job:      job,
			Step:     name,
			Line:     line,
			Rule:     RuleNoRestoreKeys,
			Severity: SevInfo,
			Message:  "no restore-keys — a key miss falls back to nothing instead of the newest prior cache",
			Suggestion: "Add a restore-keys prefix so a partial match still restores, e.g.\n" +
				"    restore-keys: |\n      ${{ runner.os }}-deps-",
		})
	}
	return out
}

// cacheSnippet renders a ready-to-paste actions/cache step for an ecosystem.
func cacheSnippet(e ecosystem) string {
	if e.setupCacheValue != "" {
		return fmt.Sprintf("Either enable the setup action's built-in cache:\n"+
			"    - uses: %s@v4\n      with:\n        cache: %s\n"+
			"  or add an explicit cache step:\n%s",
			e.setupAction, e.setupCacheValue, explicitCache(e))
	}
	return "Add an actions/cache step before the install:\n" + explicitCache(e)
}

func explicitCache(e ecosystem) string {
	var b strings.Builder
	b.WriteString("    - uses: actions/cache@v4\n")
	b.WriteString("      with:\n")
	b.WriteString("        path: |\n")
	for _, p := range e.cachePaths {
		fmt.Fprintf(&b, "          %s\n", p)
	}
	fmt.Fprintf(&b, "        key: ${{ runner.os }}-%s-${{ hashFiles('**/%s') }}\n", e.name, e.lockfile)
	fmt.Fprintf(&b, "        restore-keys: |\n          ${{ runner.os }}-%s-", e.name)
	return b.String()
}

// --- small yaml.Node helpers ---

// mapValue returns the value node for key in a mapping node, or nil.
func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// isFalse reports whether a scalar node is the boolean/string false.
func isFalse(n *yaml.Node) bool {
	return n != nil && strings.EqualFold(strings.TrimSpace(n.Value), "false")
}

// stepName returns a human label for a step: its `name:`, else its uses/run.
func stepName(step *yaml.Node, fallback string) string {
	if nm := mapValue(step, "name"); nm != nil && nm.Value != "" {
		return nm.Value
	}
	fallback = strings.TrimSpace(fallback)
	if i := strings.IndexByte(fallback, '\n'); i >= 0 {
		fallback = fallback[:i]
	}
	if len(fallback) > 50 {
		fallback = fallback[:50] + "…"
	}
	return fallback
}
