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
	return Row{Name: c.Name, Func: c.Func, Reference: c.Reference, Parity: c.Parity, Allocs: allocs, Refs: c.refRows()}
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
// the two sides (Go first on even rounds, reference first on odd ones); with
// extra references (Case.Refs) the sides are rotated instead, and Rounds is
// rounded up to a multiple of the number of sides; the load
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
		for _, r := range c.Refs {
			if r.Timer == nil {
				return nil, fmt.Errorf("case %q: reference %s has no timer", c.Name, r.ID)
			}
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
	if err := scoreAccuracy(c, &row); err != nil {
		return row, err
	}
	g, f := goTimer(c), c.Ref
	var err error
	if row.GoIters, err = Calibrate(g, opt.MinTime); err != nil {
		return row, err
	}
	if row.RefIters, err = Calibrate(f, opt.MinTime); err != nil {
		return row, err
	}
	// The sides, in the order of the registry: Go, the primary, the extra ones.
	// Round r runs them in the order williamsOrders(k)[r mod period]: every
	// side takes every position equally often AND every ordered pair of sides
	// is adjacent equally often, so that no side always follows the same other
	// one (a plain rotation balances positions, not that first-order carryover).
	// With two sides this is the alternation of schema 1 (Go first on even
	// rounds).
	sides := []roundSide{
		{id: SideGo, t: g, iters: row.GoIters},
		{id: SidePrimary, t: f, iters: row.RefIters},
	}
	for i, r := range c.Refs {
		it, err := Calibrate(r.Timer, opt.MinTime)
		if err != nil {
			return row, fmt.Errorf("%s: %w", r.ID, err)
		}
		row.Refs[i].Iters = it
		sides = append(sides, roundSide{id: r.ID, t: r.Timer, iters: it})
	}
	k := len(sides)
	orders := williamsOrders(k)
	rounds := opt.Rounds
	if p := len(orders); k > 2 && rounds%p != 0 {
		rounds += p - rounds%p
	}
	for r := 0; r < rounds; {
		load, err := opt.gate(ctx)
		if err != nil {
			return row, err
		}
		order := orders[r%len(orders)]
		rd := Round{LoadBefore: load, GoFirst: before(order, 0, 1)}
		if k > 2 {
			rd.RefsNs = make(map[string]float64, k-2)
		}
		for p := 0; p < k; p++ {
			s := sides[order[p]]
			d, err := s.t(s.iters)
			if err != nil {
				return row, err
			}
			ns := float64(d.Nanoseconds()) / float64(s.iters)
			switch s.id {
			case SideGo:
				rd.GoNs = ns
			case SidePrimary:
				rd.RefNs = ns
			default:
				rd.RefsNs[s.id] = ns
			}
			if k > 2 {
				rd.Order = append(rd.Order, s.id)
			}
		}
		if rd.LoadAfter, err = opt.Load(); err != nil {
			return row, fmt.Errorf("cannot read the load average: %w", err)
		}
		if rd.LoadAfter >= opt.MaxLoad {
			// Discarded for every side; the same round (same side order) is run
			// again once the gate is open.
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

// williamsOrders returns the run orders of k sides over one period: a Williams
// design (a balanced Latin square). Row i is (s[j] + i) mod k for the zigzag
// s = 0, 1, k-1, 2, k-2, ...; for even k the k rows balance positions and
// every ordered pair of sides is adjacent exactly once; for odd k the rows and
// their reversals (2k rows) do. k = 2 gives {0,1}, {1,0}.
func williamsOrders(k int) [][]int {
	s := make([]int, k)
	lo, hi := 1, k-1
	for j := 1; j < k; j++ {
		if j%2 == 1 {
			s[j] = lo
			lo++
		} else {
			s[j] = hi
			hi--
		}
	}
	var out [][]int
	for i := 0; i < k; i++ {
		row := make([]int, k)
		for j := range row {
			row[j] = (s[j] + i) % k
		}
		out = append(out, row)
	}
	if k%2 == 1 {
		for i := 0; i < k; i++ {
			rev := make([]int, k)
			for j := range rev {
				rev[j] = out[i][k-1-j]
			}
			out = append(out, rev)
		}
	}
	return out
}

// before reports whether side a runs before side b in order.
func before(order []int, a, b int) bool {
	for _, s := range order {
		if s == a {
			return true
		}
		if s == b {
			return false
		}
	}
	return false
}

type roundSide struct {
	id    string
	t     Timer
	iters int
}

// scoreAccuracy evaluates Go, the primary reference and every Ref with an Eval
// on the case's points and stores the results in the row. It is deterministic,
// so it is not gated; an evaluation error stops the run (a series with a hole
// is biased).
func scoreAccuracy(c Case, row *Row) error {
	a := c.Accuracy
	if a == nil {
		return nil
	}
	score := func(who string, eval EvalFunc) (*AccResult, error) {
		if eval == nil {
			return nil, nil
		}
		got, err := eval()
		if err != nil {
			return nil, fmt.Errorf("accuracy of %s: %w", who, err)
		}
		res := a.Score(got)
		if !res.valid() {
			return nil, fmt.Errorf("accuracy of %s: the scorer returned an out-of-range result", who)
		}
		return &res, nil
	}
	var err error
	if row.GoAccuracy, err = score("Go", a.Go); err != nil {
		return err
	}
	if row.PrimaryAccuracy, err = score(SidePrimary, c.RefEval); err != nil {
		return err
	}
	row.GoBudget = a.Budget
	for i, r := range c.Refs {
		if row.Refs[i].Accuracy, err = score(r.ID, r.Eval); err != nil {
			return err
		}
	}
	return nil
}
