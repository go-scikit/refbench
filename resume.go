package refbench

import (
	"errors"
	"fmt"
	"maps"
	"runtime"
)

// Pending returns the names of the cases a resume has to measure: the cases
// whose row in prev is not valid, or that have no row. A nil prev (or a
// deferred, allocation-only one) means every case.
func Pending(prev *Report, cases []Case) map[string]bool {
	todo := map[string]bool{}
	rows := map[string]Row{}
	if prev != nil && prev.Status == StatusMeasured {
		for _, r := range prev.Rows {
			rows[r.Name] = r
		}
	}
	for _, c := range cases {
		if r, ok := rows[c.Name]; !ok || !RowValid(r, prev.MaxLoad) {
			todo[c.Name] = true
		}
	}
	return todo
}

// CheckResume refuses a resume that would mix measurements that are not
// comparable: the previous report must be a measured one of the same commit,
// the same Go toolchain, the same reference versions and the same gate as the
// run about to start. The message names the field and both values.
func CheckResume(prev *Report, opt Options) error {
	opt = opt.withDefaults()
	switch {
	case prev == nil:
		return errors.New("-perf.resume: no previous perf.json to resume")
	case prev.Status != StatusMeasured:
		return fmt.Errorf("-perf.resume: the recorded report is %q, not measured: run -perf.run first", prev.Status)
	case prev.Commit != opt.Commit:
		return fmt.Errorf("-perf.resume: perf.json was measured at commit %q, this run is at %q (give the same -perf.commit, or run -perf.run again)", prev.Commit, opt.Commit)
	case prev.Go != runtime.Version():
		return fmt.Errorf("-perf.resume: perf.json was measured with %s, this binary is %s", prev.Go, runtime.Version())
	case !maps.Equal(prev.RefVersions, opt.RefVersions):
		return fmt.Errorf("-perf.resume: perf.json was measured with reference versions %v, this run has %v", prev.RefVersions, opt.RefVersions)
	case prev.MaxLoad != opt.MaxLoad:
		return fmt.Errorf("-perf.resume: perf.json was measured with gate %g, this run has %g", prev.MaxLoad, opt.MaxLoad)
	}
	return nil
}

// Merge returns prev with the rows of next that are valid substituted by name
// (order of prev's rows kept); the retries of both are added. next is the
// report of a run restricted to Pending rows, under the same CheckResume
// conditions, which Merge verifies on the headers it can see. The date is
// next's; the load statistics are recomputed over every kept round.
func Merge(prev, next *Report) (*Report, error) {
	switch {
	case prev.Status != StatusMeasured || next.Status != StatusMeasured:
		return nil, errors.New("merge: both reports must be measured")
	case prev.Commit != next.Commit || prev.Go != next.Go || prev.MaxLoad != next.MaxLoad || !maps.Equal(prev.RefVersions, next.RefVersions):
		return nil, errors.New("merge: the reports differ in commit, Go version, reference versions or gate")
	}
	got := map[string]Row{}
	for _, r := range next.Rows {
		got[r.Name] = r
	}
	out := *prev
	out.Date = next.Date
	out.Rows = make([]Row, 0, len(prev.Rows))
	for _, r := range prev.Rows {
		if n, ok := got[r.Name]; ok {
			delete(got, r.Name)
			n.Retries += r.Retries
			if RowValid(n, next.MaxLoad) {
				r = n
			} else {
				// Still deferred: keep the previous (deferred) row, count the retries.
				r.Retries = n.Retries
				r.Allocs = n.Allocs
			}
		}
		out.Rows = append(out.Rows, r)
	}
	for _, r := range next.Rows { // rows prev did not have, in next's order
		if _, ok := got[r.Name]; ok {
			out.Rows = append(out.Rows, r)
		}
	}
	out.Finish()
	return &out, nil
}

// MergeAllocs is the recording of allocation counts over an existing report:
// when prev is a measured report its timings are kept and only the allocation
// counts (and the registry's func, reference and parity) are updated from rec;
// a case with no row yet gets a deferred one, and a row with no case is dropped.
// Without a measured prev, rec is returned as it is.
func MergeAllocs(prev, rec *Report) *Report {
	if prev == nil || prev.Status != StatusMeasured {
		return rec
	}
	old := map[string]Row{}
	for _, r := range prev.Rows {
		old[r.Name] = r
	}
	out := *prev
	out.Rows = make([]Row, 0, len(rec.Rows))
	for _, r := range rec.Rows {
		if o, ok := old[r.Name]; ok {
			o.Func, o.Reference, o.Parity, o.Allocs = r.Func, r.Reference, r.Parity, r.Allocs
			r = o
		}
		out.Rows = append(out.Rows, r)
	}
	out.Finish()
	return &out
}
