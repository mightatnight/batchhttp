package batchhttp

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// FormatOptions controls human-readable output formatting.
type FormatOptions struct {
	// IncludeHeader prints a header line before run lines.
	IncludeHeader bool

	// IncludeSummary prints the summary section.
	IncludeSummary bool

	// MaxURLLen truncates URLs longer than this length (0 = no truncation).
	MaxURLLen int

	// ShowLabel includes the result Label column when present.
	ShowLabel bool

	// ShowIndex shows Index (0-based) column.
	ShowIndex bool

	// ShowRun shows Run (1-based for RunRepeated) column.
	ShowRun bool

	// TimeFormat controls formatting of durations:
	//   - "ms" renders as milliseconds with 3 decimals (e.g. 123.456ms)
	//   - "s" renders as seconds with 3 decimals (e.g. 0.123s)
	//   - otherwise uses Go's duration String() (e.g. 123ms, 1.2s)
	TimeFormat string
}

// DefaultFormatOptions returns options suitable for typical CLI output.
func DefaultFormatOptions() FormatOptions {
	return FormatOptions{
		IncludeHeader:  true,
		IncludeSummary: true,
		MaxURLLen:      120,
		ShowLabel:      true,
		ShowIndex:      true,
		ShowRun:        true,
		TimeFormat:     "ms",
	}
}

// WriteText writes a readable report for results + summary.
func WriteText(w io.Writer, results []RunResult, summary Summary, opt FormatOptions) error {
	if w == nil {
		return fmt.Errorf("nil writer")
	}

	// Header
	if opt.IncludeHeader {
		if _, err := fmt.Fprintln(w, formatHeader(opt)); err != nil {
			return err
		}
	}

	// Rows
	for _, r := range results {
		if _, err := fmt.Fprintln(w, FormatRunLine(r, opt)); err != nil {
			return err
		}
	}

	// Summary
	if opt.IncludeSummary {
		if len(results) > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		for _, line := range FormatSummaryLines(summary, opt) {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}

	return nil
}

// FormatRunLine renders one run in a compact, stable format.
func FormatRunLine(r RunResult, opt FormatOptions) string {
	var cols []string

	if opt.ShowLabel {
		cols = append(cols, formatLabel(r.Label))
	}
	if opt.ShowRun {
		if r.Run > 0 {
			cols = append(cols, fmt.Sprintf("run=%d", r.Run))
		} else {
			cols = append(cols, "run=-")
		}
	}
	if opt.ShowIndex {
		cols = append(cols, fmt.Sprintf("idx=%d", r.Index))
	}

	cols = append(cols, fmt.Sprintf("method=%s", safeMethod(r.Method)))
	cols = append(cols, fmt.Sprintf("status=%s", formatStatus(r.StatusCode, r.Err)))
	cols = append(cols, fmt.Sprintf("dur=%s", formatDuration(r.Duration, opt.TimeFormat)))

	url := r.URL
	if opt.MaxURLLen > 0 {
		url = truncate(url, opt.MaxURLLen)
	}
	if url != "" {
		cols = append(cols, fmt.Sprintf("url=%s", url))
	}

	if r.Err != nil {
		cols = append(cols, fmt.Sprintf("err=%s", sanitizeErr(r.Err)))
	}

	return strings.Join(cols, " ")
}

func formatHeader(opt FormatOptions) string {
	var cols []string

	if opt.ShowLabel {
		cols = append(cols, "label")
	}
	if opt.ShowRun {
		cols = append(cols, "run")
	}
	if opt.ShowIndex {
		cols = append(cols, "idx")
	}
	cols = append(cols, "method", "status", "dur", "url", "err")

	return strings.Join(cols, "\t")
}

// FormatSummaryLines renders the summary into multiple lines (for CLI printing).
func FormatSummaryLines(s Summary, opt FormatOptions) []string {
	_ = opt // reserved for future (e.g., units)

	lines := make([]string, 0, 8)

	lbl := s.Label
	if lbl == "" {
		lbl = "-"
	}

	lines = append(lines, "====================")
	lines = append(lines, "BATCH SUMMARY")
	lines = append(lines, "====================")
	lines = append(lines, fmt.Sprintf("label:          %s", lbl))
	lines = append(lines, fmt.Sprintf("total:          %d", s.Total))
	lines = append(lines, fmt.Sprintf("successful:     %d", s.Success))
	lines = append(lines, fmt.Sprintf("failed:         %d", s.Failed))
	lines = append(lines, fmt.Sprintf("success_rate:   %.1f%%", s.SuccessRate*100))
	lines = append(lines, fmt.Sprintf("total_wall:     %s", formatDuration(s.TotalDuration, "s")))

	// Only include latency stats if they were computed (Min==0 and Max==0 can be legitimate,
	// but in our runner we only compute these when successful durations exist).
	if s.Success > 0 && (s.Min != 0 || s.Max != 0 || s.Avg != 0) {
		lines = append(lines, "")
		lines = append(lines, "TIMING (successful runs)")
		lines = append(lines, fmt.Sprintf("avg:            %s", s.Avg))
		lines = append(lines, fmt.Sprintf("min:            %s", s.Min))
		lines = append(lines, fmt.Sprintf("p50:            %s", s.P50))
		lines = append(lines, fmt.Sprintf("p90:            %s", s.P90))
		lines = append(lines, fmt.Sprintf("p99:            %s", s.P99))
		lines = append(lines, fmt.Sprintf("max:            %s", s.Max))
	}

	return lines
}

// FormatResultsTable returns a tabular output (single string) for quick printing.
// This is intentionally simple and stable.
func FormatResultsTable(results []RunResult, opt FormatOptions) string {
	var b strings.Builder
	_ = WriteText(&b, results, Summary{}, opt) // Summary ignored by caller; they can append separately.
	return b.String()
}

// SortResultsByDuration sorts results in-place by Duration ascending.
// Useful prior to printing top-N slowest runs, etc.
func SortResultsByDuration(results []RunResult) {
	sort.Slice(results, func(i, j int) bool {
		return results[i].Duration < results[j].Duration
	})
}

func formatDuration(d time.Duration, mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "ms":
		return fmt.Sprintf("%.3fms", float64(d)/float64(time.Millisecond))
	case "s":
		return fmt.Sprintf("%.3fs", float64(d)/float64(time.Second))
	default:
		return d.String()
	}
}

func formatStatus(code int, err error) string {
	if err != nil {
		return "ERR"
	}
	if code == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", code)
}

func safeMethod(m string) string {
	if m == "" {
		return "GET"
	}
	return m
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func formatLabel(label string) string {
	if label == "" {
		return "label=-"
	}
	return "label=" + label
}

func sanitizeErr(err error) string {
	if err == nil {
		return ""
	}
	// Keep it single-line for log/CLI friendliness.
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	msg = strings.TrimSpace(msg)
	return msg
}
