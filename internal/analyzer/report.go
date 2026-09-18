package analyzer

import (
	"encoding/json"
	"fmt"
	"io"
)

// WriteJSON renders the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText renders findings as human-readable blocks plus a summary line.
func (r *Report) WriteText(w io.Writer) error {
	for _, f := range r.Findings {
		if _, err := fmt.Fprintf(w, "%s  %s:%d  [%s]  job=%q step=%q\n  %s\n",
			f.Severity, f.File, f.Line, f.Rule, f.Job, f.Step, f.Message); err != nil {
			return err
		}
		if f.Suggestion != "" {
			if _, err := fmt.Fprintf(w, "  suggested fix:\n%s\n", indent(f.Suggestion)); err != nil {
				return err
			}
		}
		fmt.Fprintln(w)
	}
	_, err := fmt.Fprintf(w, "%d file(s) scanned — %d warning(s), %d info\n",
		r.FilesCount, r.WarnCount, r.InfoCount)
	return err
}

// indent shifts a multi-line suggestion under the finding.
func indent(s string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += "  " + line + "\n"
	}
	return out[:len(out)-1] // drop trailing newline
}

func splitLines(s string) []string {
	var lines []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	lines = append(lines, cur)
	return lines
}
