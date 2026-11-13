package refbench

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Issue is a drafted issue: refbench drafts, a person files (go-fleettools'
// ghissue does the filing).
type Issue struct {
	Case   string
	Title  string
	Labels []string
	Body   string
}

// ErrInvalidRun is returned by Issues for a report that gives no verdict.
var ErrInvalidRun = errors.New("the run gives no verdict: nothing to file")

// Issues drafts one issue per slower row of a valid, measured report.
func Issues(rep *Report) ([]Issue, error) {
	rep.Finish()
	switch {
	case rep.Status != StatusMeasured:
		return nil, fmt.Errorf("%w (status %s)", ErrInvalidRun, rep.Status)
	case rep.ValidRows() == 0:
		return nil, fmt.Errorf("%w (no row has all its rounds under the load gate)", ErrInvalidRun)
	}
	var out []Issue
	for _, row := range rep.Rows {
		slower := slowerSides(row)
		if !row.Valid || len(slower) == 0 {
			continue
		}
		// One issue per case, titled after the fastest side Go is slower than:
		// a gap against ANY faster reference is a defect, even a less accurate
		// one (a "tradeoff" is labelled, not excused).
		target := slower[0]
		for _, s := range slower[1:] {
			if s.sum.Median < target.sum.Median {
				target = s
			}
		}
		out = append(out, Issue{
			Case:   row.Name,
			Title:  fmt.Sprintf("perf: %s is %.2fx slower than %s on one core", row.Name, target.ratio, target.name),
			Labels: []string{"perf"},
			Body:   issueBody(rep, row),
		})
	}
	return out, nil
}

func issueBody(rep *Report, row Row) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Case `%s` (public function `%s`) against %s, one core, parity %s.\n\n", row.Name, row.Func, row.Reference, row.Parity)
	b.WriteString("| | median ns/call | first quartile | third quartile |\n|---|---:|---:|---:|\n")
	fmt.Fprintf(&b, "| Go | %.4g | %.4g | %.4g |\n", row.Go.Median, row.Go.Q1, row.Go.Q3)
	fmt.Fprintf(&b, "| reference | %.4g | %.4g | %.4g |\n\n", row.Ref.Median, row.Ref.Q1, row.Ref.Q3)
	fmt.Fprintf(&b, "Ratio Go / reference: **%.2f**. Allocations per call (Go): %g.\n\n", row.Ratio, row.Allocs)
	if len(row.Refs) > 0 {
		b.WriteString("Every reference of the case (ratio = Go / reference; a reference that is faster is a defect even when it is less accurate):\n\n")
		b.WriteString("| reference | license | median ns/call | ratio | verdict | label | max ulp | coverage |\n|---|---|---:|---:|---|---|---:|---:|\n")
		fmt.Fprintf(&b, "| %s (primary) | | %.4g | %.2f | %s | %s | %s | %s |\n", esc(row.Reference), row.Ref.Median, row.Ratio, row.Verdict,
			Label(row.Verdict, row.GoAccuracy, row.PrimaryAccuracy, row.GoBudget), ulpCell(row.PrimaryAccuracy), covCell(row.PrimaryAccuracy))
		for _, ref := range row.Refs {
			fmt.Fprintf(&b, "| %s (`%s`) | %s | %.4g | %.2f | %s | %s | %s | %s |\n", esc(ref.label()), ref.ID, ref.License, ref.Summary.Median, ref.Ratio, ref.Verdict, ref.Label, ulpCell(ref.Accuracy), covCell(ref.Accuracy))
		}
		fmt.Fprintf(&b, "\nGo: max ulp %s", ulpCell(row.GoAccuracy))
		if row.GoBudget > 0 {
			fmt.Fprintf(&b, " (stated bound %g)", row.GoBudget)
		}
		b.WriteString(". The goal is to be faster without giving up accuracy.\n\n")
	}
	fmt.Fprintf(&b, "- Commit: %s (%s)\n- Date: %s\n- Go: %s\n- Machine: %s\n", rep.Commit, rep.Repo, rep.Date, rep.Go, rep.Machine)
	keys := make([]string, 0, len(rep.RefVersions))
	for k := range rep.RefVersions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- %s: %s\n", k, rep.RefVersions[k])
	}
	fmt.Fprintf(&b, "- Gate: 1-minute load at most %.2f\n\nLoad of every round (before, after; Go first?):\n\n", rep.MaxLoad)
	for i, rd := range row.Rounds {
		fmt.Fprintf(&b, "%d. %.2f, %.2f, %t\n", i+1, rd.LoadBefore, rd.LoadAfter, rd.GoFirst)
	}
	return b.String()
}

// Slug returns a file name for a case: letters and digits kept, anything else a dash.
func Slug(name string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}
