package refbench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/go-fleettools/fleettools/loadavg"
)

// On-load policies.
const (
	// OnLoadWait waits for the load to drop, up to Options.LoadTimeout.
	OnLoadWait = "wait"
	// OnLoadRefuse checks once and refuses.
	OnLoadRefuse = "refuse"
)

// DefaultMaxLoad is the design's gate: no timing at a 1-minute load of 8 or more.
const DefaultMaxLoad = 8.0

// DefaultMaxRetries is the per-row cap of rounds re-run after ending above the gate.
const DefaultMaxRetries = 10

// Options configures Run. The zero value is usable: 7 rounds, 100 ms samples,
// gate 8, wait up to 30 minutes.
type Options struct {
	Rounds      int
	MinTime     time.Duration
	MaxLoad     float64
	OnLoad      string
	LoadTimeout time.Duration
	Poll        time.Duration
	// MaxRetries caps, per row, the rounds that may end above the gate and be
	// re-run; one more and the row is deferred (0 means DefaultMaxRetries).
	MaxRetries int
	// Only, when not nil, restricts the run to the named cases (a resume).
	Only map[string]bool
	// Load reads the 1-minute load; nil reads the host's (loadavg.Avg1).
	Load func() (float64, error)
	// Now is the clock for the report's date; nil is time.Now.
	Now func() time.Time

	Commit      string
	Repo        string
	Machine     string
	RefVersions map[string]string
}

func (o Options) withDefaults() Options {
	if o.Rounds <= 0 {
		o.Rounds = 7
	}
	if o.MinTime <= 0 {
		o.MinTime = 100 * time.Millisecond
	}
	if o.MaxLoad <= 0 {
		o.MaxLoad = DefaultMaxLoad
	}
	if o.OnLoad == "" {
		o.OnLoad = OnLoadWait
	}
	if o.LoadTimeout <= 0 {
		o.LoadTimeout = 30 * time.Minute
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = DefaultMaxRetries
	}
	if o.Poll <= 0 {
		o.Poll = 10 * time.Second
	}
	if o.Load == nil {
		o.Load = loadavg.Avg1
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Machine == "" {
		o.Machine = fmt.Sprintf("%s/%s, %d CPUs", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	}
	return o
}

// ErrOnLoad is wrapped by the error of a refused or timed-out gate: the run has
// no verdict.
var ErrOnLoad = errors.New("the load gate did not open")

// gate returns the load once it is at most MaxLoad, per the policy.
func (o Options) gate(ctx context.Context) (float64, error) {
	timeout := o.LoadTimeout
	if o.OnLoad == OnLoadRefuse {
		timeout = 0
	}
	// "At or above 8 waits": loadavg's limit is inclusive, so ask for just below.
	l, err := loadavg.WaitFunc(ctx, o.Load, math.Nextafter(o.MaxLoad, 0), timeout, o.Poll, nil)
	if err != nil {
		return l, fmt.Errorf("%w: %w", ErrOnLoad, err)
	}
	return l, nil
}

func (o Options) header(status string) *Report {
	return &Report{
		Schema:      SchemaVersion,
		Status:      status,
		Date:        o.Now().Format(time.RFC3339),
		Commit:      o.Commit,
		Repo:        o.Repo,
		Go:          runtime.Version(),
		Machine:     o.Machine,
		RefVersions: o.RefVersions,
		MaxLoad:     o.MaxLoad,
		Valid:       true,
	}
}

// AllocsPerCall returns the allocations per call of the Go body: the count of
// AllocIters calls, as testing.AllocsPerRun counts them, divided by AllocIters
// (exact under any load).
func AllocsPerCall(c Case) float64 {
	n := max(c.AllocIters, 1)
	return testing.AllocsPerRun(50, func() { c.Go(n) }) / float64(n)
}

func (c Case) row(allocs float64) Row {
	return Row{Name: c.Name, Func: c.Func, Reference: c.Reference, Parity: c.Parity, Allocs: allocs}
}

// RecordAllocs returns a deferred report: the allocation counts of every case
// and no timing. It is what a repository commits when the host's load rules
// timing out, so that the CI mode has numbers to check.
func RecordAllocs(cases []Case, opt Options) (*Report, error) {
	if err := Validate(cases); err != nil {
		return nil, err
	}
	opt = opt.withDefaults()
	rep := opt.header(StatusDeferred)
	for _, c := range cases {
		rep.Rows = append(rep.Rows, c.row(AllocsPerCall(c)))
	}
	rep.Finish()
	return rep, nil
}

func goTimer(c Case) Timer {
	return func(iters int) (time.Duration, error) {
		t0 := time.Now()
		c.Go(iters)
		return time.Since(t0), nil
	}
}

// Run measures every case. The Go side runs under GOMAXPROCS=1, set before and
// restored after, never inside a timed interval. Rounds alternate the order of
// the two sides (Go first on even rounds, reference first on odd ones); the load
// is read, gated and stored before every round and read again after it. A
// reference error stops the run: a series with a hole is biased, not smaller.
//
// A round that ends above the gate is discarded and re-run (the next gate
// check waits, within LoadTimeout, until the load is back under the gate; the
// round keeps its position in the alternation), at most MaxRetries times per
// row; past the cap the row is deferred, alone. A gate that does not open stops
// the run: the rows measured so far are valid, the current and the remaining
// rows are deferred. A run that measured no row at all returns ErrOnLoad and
// no report. Rows are valid on their own (see Row.Valid); Report.Deferred
// names the others, and Merge combines a later run of those with this one.
func Run(ctx context.Context, cases []Case, opt Options) (*Report, error) {
	if err := Validate(cases); err != nil {
		return nil, err
	}
	for _, c := range cases {
		if c.Ref == nil {
			return nil, fmt.Errorf("case %q has no reference timer", c.Name)
		}
	}
	opt = opt.withDefaults()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	rep := opt.header(StatusMeasured)
	var closed error // the gate error that stopped the run, if any
	for _, c := range cases {
		if opt.Only != nil && !opt.Only[c.Name] {
			continue
		}
		if closed != nil {
			rep.Rows = append(rep.Rows, c.row(AllocsPerCall(c)))
			continue
		}
		row, err := runCase(ctx, c, opt)
		switch {
		case errors.Is(err, errRetryCap):
			row.Rounds = nil
		case errors.Is(err, ErrOnLoad):
			row.Rounds, closed = nil, err
		case err != nil:
			return nil, fmt.Errorf("case %q: %w", c.Name, err)
		}
		rep.Rows = append(rep.Rows, row)
	}
	rep.Finish()
	if rep.ValidRows() == 0 && len(rep.Rows) > 0 {
		if closed == nil {
			closed = fmt.Errorf("%w: every row ended above the gate more than %d times", ErrOnLoad, opt.MaxRetries)
		}
		return nil, closed
	}
	return rep, nil
}

// errRetryCap marks a row whose rounds kept ending above the gate.
var errRetryCap = errors.New("rounds kept ending above the load gate")

func runCase(ctx context.Context, c Case, opt Options) (Row, error) {
	row := c.row(AllocsPerCall(c))
	g, f := goTimer(c), c.Ref
	var err error
	if row.GoIters, err = Calibrate(g, opt.MinTime); err != nil {
		return row, err
	}
	if row.RefIters, err = Calibrate(f, opt.MinTime); err != nil {
		return row, err
	}
	for r := 0; r < opt.Rounds; {
		load, err := opt.gate(ctx)
		if err != nil {
			return row, err
		}
		rd := Round{LoadBefore: load, GoFirst: r%2 == 0}
		sides := [2]struct {
			t     Timer
			iters int
			out   *float64
		}{{g, row.GoIters, &rd.GoNs}, {f, row.RefIters, &rd.RefNs}}
		if !rd.GoFirst {
			sides[0], sides[1] = sides[1], sides[0]
		}
		for _, s := range sides {
			d, err := s.t(s.iters)
			if err != nil {
				return row, err
			}
			*s.out = float64(d.Nanoseconds()) / float64(s.iters)
		}
		if rd.LoadAfter, err = opt.Load(); err != nil {
			return row, fmt.Errorf("cannot read the load average: %w", err)
		}
		if rd.LoadAfter >= opt.MaxLoad {
			// Discarded; the same round (same side order) is run again once the
			// gate is open.
			row.Retries++
			if row.Retries > opt.MaxRetries {
				return row, errRetryCap
			}
			continue
		}
		row.Rounds = append(row.Rounds, rd)
		r++
	}
	return row, nil
}
