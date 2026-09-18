// Command gha-cache-optimizer scans GitHub Actions workflow files for missing
// or ineffective dependency caching and prints ready-to-paste fixes.
//
// Exit codes:
//
//	0  no warnings (info-only findings still allowed)
//	1  at least one warning-level finding (CI gate)
//	2  runtime error (bad path, unreadable file, flags)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/moveeeax/gha-cache-optimizer/internal/analyzer"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("gha-cache-optimizer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: gha-cache-optimizer [--json] [PATH ...]\n\n")
		fmt.Fprintf(stderr, "Scans workflow YAML for missing/ineffective actions/cache config.\n")
		fmt.Fprintf(stderr, "PATH defaults to .github/workflows. A directory is scanned for *.yml/*.yaml;\n")
		fmt.Fprintf(stderr, "individual files are scanned directly.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{filepath.Join(".github", "workflows")}
	}

	rep, err := scan(paths)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}

	if *asJSON {
		if err := rep.WriteJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
	} else {
		if err := rep.WriteText(stdout); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
	}

	if rep.WarnCount > 0 {
		return 1
	}
	return 0
}

// scan resolves each path to either a directory scan or a single-file scan and
// merges the results.
func scan(paths []string) (*analyzer.Report, error) {
	merged := &analyzer.Report{}
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			rep, err := analyzer.ScanDir(p)
			if err != nil {
				return nil, err
			}
			merged.Findings = append(merged.Findings, rep.Findings...)
			merged.FilesCount += rep.FilesCount
			merged.WarnCount += rep.WarnCount
			merged.InfoCount += rep.InfoCount
		} else {
			files = append(files, p)
		}
	}
	if len(files) > 0 {
		rep, err := analyzer.ScanFiles(files)
		if err != nil {
			return nil, err
		}
		merged.Findings = append(merged.Findings, rep.Findings...)
		merged.FilesCount += rep.FilesCount
		merged.WarnCount += rep.WarnCount
		merged.InfoCount += rep.InfoCount
	}
	return merged, nil
}
