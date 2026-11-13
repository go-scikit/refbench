package refbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// SchemaMultiRef is the schema of a perf.json whose rows carry extra
// references. A repository with no Case.Refs keeps writing SchemaVersion (1).
const SchemaMultiRef = 2

// Reserved side names in a Round's Order and in Row.Best.
const (
	SideGo      = "go"
	SidePrimary = "primary"
)

// EvalFunc evaluates a function once on the case's accuracy points and returns
// one value per point. A point the reference refuses (a domain error) is NaN;
// the worker counts it.
type EvalFunc func() ([]float64, error)

// Ref is one additional reference of a case. The primary reference stays
// Case.Ref (SciPy today): its tables and sentences keep their meaning.
type Ref struct {
	// ID is the stable key "<kind>:<name>", e.g. "julia:Bessels.besseli",
	// "boost:cyl_bessel_i", "fortran:amos.zbesi". The kind is a worker family.
	ID string
	// Name is the label of the table; empty means ID.
	Name string
	// License is an SPDX identifier, or "GPL-black-box" for a reference that is
	// only ever run, never read or linked (kinds "r" and "gsl").
	License string
	// Timer times the reference inside its own process.
	Timer Timer
	// Eval scores the reference's accuracy; nil gives it no accuracy column.
	Eval EvalFunc
}

// Accuracy scores Go and every Ref that has an Eval on the same points, with
// the repository's own scorer (special: ulp against the mpmath hi+lo fixtures).
type Accuracy struct {
	// Go evaluates the Go function on the points.
	Go EvalFunc
	// Score returns the error of one evaluation. Judge and subject count alike:
	// the same points, the same ulp function, the same treatment of non-finite
	// values, for every side.
	Score func(got []float64) AccResult
	// Budget is the maximum ulp Go promises (its stated bound). It separates a
	// "behind" (the reference is as accurate as Go promises) from a "tradeoff"
	// (the reference is less accurate than that). Zero means Go's own measured
	// maximum.
	Budget float64
}

// ULP is an error in units in the last place. It is +Inf when a finite value
// was expected and NaN or Inf came back, which JSON cannot carry: it is the
// string "inf" there.
type ULP float64

// MarshalJSON encodes a finite value as a number and +Inf as "inf".
func (u ULP) MarshalJSON() ([]byte, error) {
	f := float64(u)
	switch {
	case math.IsInf(f, 1):
		return []byte(`"inf"`), nil
	case math.IsNaN(f) || math.IsInf(f, -1) || f < 0:
		return nil, errors.New("ulp is not a non-negative number or +Inf")
	}
	return json.Marshal(f)
}

// UnmarshalJSON reads a number or the string "inf".
func (u *ULP) UnmarshalJSON(b []byte) error {
	if string(b) == `"inf"` {
		*u = ULP(math.Inf(1))
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("ulp: %w", err)
	}
	*u = ULP(f)
	return nil
}

// AccResult is the accuracy of one side on the case's points.
type AccResult struct {
	// MaxULP is the honest worst case: +Inf if a finite value was expected and
	// NaN or Inf was returned.
	MaxULP ULP `json:"max_ulp"`
	// MaxRegion says where the maximum occurs.
	MaxRegion string `json:"max_region,omitempty"`
	// MaxULPFin is the maximum over the points where the side returned a finite
	// value, to be read with Coverage.
	MaxULPFin ULP `json:"max_ulp_fin"`
	// Coverage is the fraction of points the side evaluated (exceptions count
	// as not evaluated), in [0, 1].
	Coverage float64 `json:"coverage"`
	// Points is the number of points scored.
	Points int `json:"points"`
	// Disagree counts the points where the side returned a non-finite value
	// where a finite one was expected, or the reverse (the points that make
	// MaxULP infinite). Reported, never judged.
	Disagree int `json:"disagree,omitempty"`
}

func (a AccResult) valid() bool {
	m, f := float64(a.MaxULP), float64(a.MaxULPFin)
	return !math.IsNaN(m) && m >= 0 && !math.IsNaN(f) && f >= 0 &&
		!math.IsNaN(a.Coverage) && a.Coverage >= 0 && a.Coverage <= 1 && a.Points >= 0 && a.Disagree >= 0 && a.Disagree <= a.Points
}

// RefRow is one extra reference's result in a row. ID, Name and License come
// from the registry; the rest is derived from the rounds by Finish.
type RefRow struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	License string `json:"license"`
	Iters   int    `json:"iters,omitempty"`
	// Summary, Ratio, Verdict and Label: Go against this reference.
	Summary  Summary    `json:"summary"`
	Ratio    float64    `json:"ratio,omitempty"`
	Verdict  string     `json:"verdict,omitempty"`
	Label    string     `json:"label,omitempty"`
	Accuracy *AccResult `json:"accuracy,omitempty"`
}

// Labels of a side against Go (design, section 3.2).
const (
	// LabelAhead: Go is faster or ties, and is at least as accurate.
	LabelAhead = "ahead"
	// LabelFasterLessAccurate: Go is faster or ties, and is less accurate: an
	// accuracy defect.
	LabelFasterLessAccurate = "faster-less-accurate"
	// LabelBehind: Go is slower than a reference that is as accurate as Go
	// promises: a perf defect.
	LabelBehind = "behind"
	// LabelTradeoff: Go is slower than a reference that is less accurate than
	// Go promises: still a perf defect (the fleet rule), labelled so.
	LabelTradeoff = "tradeoff"
)

// Label combines the time verdict with the accuracy. Without accuracy on both
// sides a faster Go has no label (no accuracy claim is made) and a slower Go is
// behind.
//
// The judge is MaxULPFin, the worst error over the points where each side
// returned a finite value: one special-value disagreement (MaxULP = +Inf, a
// NaN where a number was expected) must not decide between "behind" and
// "tradeoff". Those disagreements are reported separately (AccResult.Disagree,
// and MaxULP itself).
func Label(verdict string, goAcc, refAcc *AccResult, budget float64) string {
	if verdict == Slower {
		if goAcc != nil && refAcc != nil {
			if budget <= 0 {
				budget = float64(goAcc.MaxULPFin)
			}
			if float64(refAcc.MaxULPFin) > budget {
				return LabelTradeoff
			}
		}
		return LabelBehind
	}
	if verdict == "" || goAcc == nil || refAcc == nil {
		return ""
	}
	if goAcc.MaxULPFin <= refAcc.MaxULPFin {
		return LabelAhead
	}
	return LabelFasterLessAccurate
}

var refIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*:[A-Za-z0-9][A-Za-z0-9_.+-]*$`)

// RefKind returns the kind of a reference ID ("julia" for "julia:Bessels.besseli").
func RefKind(id string) string {
	k, _, _ := strings.Cut(id, ":")
	return k
}

// blackBoxKinds are the kinds whose code is GPL: run, never read or linked.
var blackBoxKinds = map[string]bool{"r": true, "gsl": true}

// validateRefs checks the registry's extra references of one case.
func validateRefs(c Case) error {
	seen := map[string]bool{}
	for _, r := range c.Refs {
		switch {
		case !refIDPattern.MatchString(r.ID):
			return fmt.Errorf("case %q: reference ID %q is not <kind>:<name>", c.Name, r.ID)
		case seen[r.ID]:
			return fmt.Errorf("case %q: reference %q is registered twice", c.Name, r.ID)
		case strings.TrimSpace(r.License) == "":
			return fmt.Errorf("case %q: reference %q has no license", c.Name, r.ID)
		case strings.Contains(strings.ToUpper(r.License), "GPL") && !blackBoxKinds[RefKind(r.ID)]:
			return fmt.Errorf("case %q: reference %q is %s but its kind %q is not a black-box kind (r, gsl)", c.Name, r.ID, r.License, RefKind(r.ID))
		}
		seen[r.ID] = true
	}
	if c.Accuracy != nil && (c.Accuracy.Go == nil || c.Accuracy.Score == nil) {
		return fmt.Errorf("case %q: Accuracy needs Go and Score", c.Name)
	}
	if c.Accuracy != nil && (math.IsNaN(c.Accuracy.Budget) || c.Accuracy.Budget < 0) {
		return fmt.Errorf("case %q: Accuracy.Budget is negative or NaN", c.Name)
	}
	return nil
}

func (c Case) refRows() []RefRow {
	if len(c.Refs) == 0 {
		return nil
	}
	out := make([]RefRow, len(c.Refs))
	for i, r := range c.Refs {
		name := r.Name
		if name == r.ID {
			name = ""
		}
		out[i] = RefRow{ID: r.ID, Name: name, License: r.License}
	}
	return out
}

// sideLabel is the table label of a RefRow.
func (r RefRow) label() string {
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

// usesMultiRef reports whether a row carries any schema 2 field: extra
// references, or accuracy (Go's, the primary's) and its budget. One predicate
// decides the schema Finish writes and what a schema 1 file may not contain.
func (row Row) usesMultiRef() bool {
	return len(row.Refs) > 0 || row.GoAccuracy != nil || row.PrimaryAccuracy != nil || row.GoBudget != 0
}

// usesRefs reports whether any row needs schema 2.
func (r *Report) usesRefs() bool {
	for _, row := range r.Rows {
		if row.usesMultiRef() {
			return true
		}
	}
	return false
}

// finishRefs derives the per-reference summaries, ratios, verdicts, labels and
// the headline (the fastest side, primary included) of one row from its rounds.
func finishRefs(row *Row) {
	row.Best, row.BestRatio, row.BestVerdict, row.BestLabel = "", 0, "", ""
	for i := range row.Refs {
		ref := &row.Refs[i]
		ref.Summary, ref.Ratio, ref.Verdict, ref.Label = Summary{}, 0, "", ""
		var xs []float64
		for _, rd := range row.Rounds {
			xs = append(xs, rd.RefsNs[ref.ID])
		}
		if len(xs) == 0 {
			continue
		}
		ref.Summary = Summarize(xs)
		ref.Ratio = row.Go.Median / ref.Summary.Median
		ref.Verdict = Judge(row.Go, ref.Summary)
		ref.Label = Label(ref.Verdict, row.GoAccuracy, ref.Accuracy, row.GoBudget)
	}
	if len(row.Refs) == 0 || len(row.Rounds) == 0 {
		return
	}
	best, bestSum, bestAcc := SidePrimary, row.Ref, row.PrimaryAccuracy
	for _, ref := range row.Refs {
		if ref.Summary.Median < bestSum.Median {
			best, bestSum, bestAcc = ref.ID, ref.Summary, ref.Accuracy
		}
	}
	row.Best = best
	row.BestRatio = row.Go.Median / bestSum.Median
	row.BestVerdict = Judge(row.Go, bestSum)
	row.BestLabel = Label(row.BestVerdict, row.GoAccuracy, bestAcc, row.GoBudget)
}

// slowerSides returns, in the report's order, the label and the summary of
// every side (primary included) that Go is slower than.
type side struct {
	id, name string
	sum      Summary
	ratio    float64
	label    string
	acc      *AccResult
}

func slowerSides(row Row) []side {
	var out []side
	if row.Verdict == Slower {
		out = append(out, side{SidePrimary, row.Reference, row.Ref, row.Ratio, Label(Slower, row.GoAccuracy, row.PrimaryAccuracy, row.GoBudget), row.PrimaryAccuracy})
	}
	for _, r := range row.Refs {
		if r.Verdict == Slower {
			out = append(out, side{r.ID, r.label(), r.Summary, r.Ratio, r.Label, r.Accuracy})
		}
	}
	return out
}

// CheckRefs checks a report against the registry: every Ref.ID declared by a
// case has an entry in the case's row (a deferred row keeps its entries), the
// row has no entry the registry does not declare, and every entry has a license.
func CheckRefs(rep *Report, cases []Case) []string {
	rows := map[string]Row{}
	for _, r := range rep.Rows {
		rows[r.Name] = r
	}
	var bad []string
	for _, c := range cases {
		row, ok := rows[c.Name]
		if !ok {
			continue // CheckAllocs reports the missing row
		}
		have := map[string]RefRow{}
		for _, r := range row.Refs {
			have[r.ID] = r
		}
		want := map[string]bool{}
		for _, r := range c.Refs {
			want[r.ID] = true
			e, ok := have[r.ID]
			switch {
			case !ok:
				bad = append(bad, fmt.Sprintf("%s: reference %s has no entry in perf.json (record it)", c.Name, r.ID))
			case e.License != r.License:
				bad = append(bad, fmt.Sprintf("%s: reference %s is licensed %q in perf.json, %q in the registry", c.Name, r.ID, e.License, r.License))
			}
		}
		for id := range have {
			if !want[id] {
				bad = append(bad, fmt.Sprintf("%s: perf.json has reference %s, which the case does not declare", c.Name, id))
			}
		}
	}
	return bad
}

// FirstDiff returns the index of the first value of a and b that differs in
// its bits, or -1. It is the check of the first multi-reference run: the
// primary reference, evaluated through the new path, must reproduce the
// fixtures' recorded column bit for bit, which proves the inputs reach the
// workers unaltered.
func FirstDiff(a, b []float64) int {
	for i := range min(len(a), len(b)) {
		if math.Float64bits(a[i]) != math.Float64bits(b[i]) {
			return i
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b))
	}
	return -1
}

// checkMultiRef validates the schema 2 fields of a row read from a file.
func checkMultiRef(row Row, schema int) error {
	if schema == SchemaVersion && row.usesMultiRef() {
		return errors.New("a schema 1 file carries extra-reference fields (schema 2 is required)")
	}
	if !finiteNonNeg(row.GoBudget) {
		return errors.New("go_budget is not a finite non-negative number")
	}
	for _, a := range []*AccResult{row.GoAccuracy, row.PrimaryAccuracy} {
		if a != nil && !a.valid() {
			return errors.New("an accuracy is out of range")
		}
	}
	seen := map[string]bool{}
	for _, r := range row.Refs {
		switch {
		case !refIDPattern.MatchString(r.ID):
			return fmt.Errorf("reference ID %q is not <kind>:<name>", r.ID)
		case seen[r.ID]:
			return fmt.Errorf("reference %q appears twice", r.ID)
		case strings.TrimSpace(r.License) == "":
			return fmt.Errorf("reference %q has no license", r.ID)
		case r.Accuracy != nil && !r.Accuracy.valid():
			return fmt.Errorf("reference %q: an accuracy is out of range", r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}

// checkRoundRefs validates the extra samples of a round against its row.
func checkRoundRefs(rd Round, row Row) error {
	if len(row.Refs) == 0 {
		if len(rd.RefsNs) > 0 || len(rd.Order) > 0 {
			return errors.New("a round has extra-reference samples but the row has no references")
		}
		return nil
	}
	if len(rd.RefsNs) != len(row.Refs) {
		return errors.New("a round does not have one sample per reference")
	}
	want := map[string]bool{SideGo: true, SidePrimary: true}
	for _, r := range row.Refs {
		ns, ok := rd.RefsNs[r.ID]
		if !ok || !finiteNonNeg(ns) || ns == 0 {
			return fmt.Errorf("a round's sample of %s is not a finite positive time", r.ID)
		}
		want[r.ID] = true
	}
	if len(rd.Order) != len(want) {
		return errors.New("a round's order is not a permutation of its sides")
	}
	got := map[string]bool{}
	for _, s := range rd.Order {
		if !want[s] || got[s] {
			return fmt.Errorf("a round's order names %q twice or not at all", s)
		}
		got[s] = true
	}
	return nil
}

// sameRefIDs reports whether two reference lists name the same IDs in the same order.
func sameRefIDs(a, b []RefRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

// keepRefData returns old with the registry's identity (name, license) of cur.
// The lists have the same IDs (sameRefIDs).
func keepRefData(old, cur []RefRow) []RefRow {
	out := append([]RefRow(nil), old...)
	for i := range out {
		out[i].Name, out[i].License = cur[i].Name, cur[i].License
	}
	return out
}
