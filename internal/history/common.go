// Package history builds reports from a repository's Actions run history:
// cache hit rate per workflow, job and key prefix (cache-report), and
// estimated runner minutes and cost per workflow, job and matrix leg (cost).
package history

import (
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Format is an output format shared by both reports.
type Format string

const (
	FormatTable Format = "table"
	FormatCSV   Format = "csv"
	FormatJSON  Format = "json"
)

// ParseFormat validates a --format value.
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(s)); f {
	case FormatTable, FormatCSV, FormatJSON:
		return f, nil
	}
	return "", fmt.Errorf("unknown format %q (want table, csv or json)", s)
}

var relSinceRe = regexp.MustCompile(`^(\d+)([hdw])$`)

// ParseSince turns "30d", "2w", "12h" or a YYYY-MM-DD date into the start of
// the report window, relative to now.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if m := relSinceRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n <= 0 {
			return time.Time{}, fmt.Errorf("--since must be positive, got %q", s)
		}
		unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		return now.Add(-time.Duration(n) * unit), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		if t.After(now) {
			return time.Time{}, fmt.Errorf("--since %s is in the future", s)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since wants 30d, 2w, 12h or YYYY-MM-DD, got %q", s)
}

// matrixSuffixRe matches the " (a, b)" GitHub appends to a matrix job's name.
var matrixSuffixRe = regexp.MustCompile(`\s+\([^()]*\)$`)

// baseJobName strips the matrix suffix: "test (1.26, ubuntu-latest)" -> "test".
func baseJobName(name string) string {
	return matrixSuffixRe.ReplaceAllString(name, "")
}

// weekStart returns the Monday (UTC) of t's ISO week, as YYYY-MM-DD.
func weekStart(t time.Time) string {
	t = t.UTC()
	offset := (int(t.Weekday()) + 6) % 7 // Monday = 0
	d := time.Date(t.Year(), t.Month(), t.Day()-offset, 0, 0, 0, 0, time.UTC)
	return d.Format("2006-01-02")
}

// table is a header plus rows, rendered as aligned text or CSV.
type table struct {
	header []string
	rows   [][]string
}

func (t table) writeText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(t.header, "\t"))
	for _, r := range t.rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

func (t table) writeCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(t.header); err != nil {
		return err
	}
	if err := cw.WriteAll(t.rows); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func fmtFloat(f float64, prec int) string { return strconv.FormatFloat(f, 'f', prec, 64) }

func fmtPct(num, den int) string {
	if den == 0 {
		return "-"
	}
	return fmtFloat(100*float64(num)/float64(den), 1)
}
