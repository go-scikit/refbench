package refbench

import (
	"math"
	"sort"
)

// Summary is the distribution of one side's samples over the rounds.
type Summary struct {
	Median float64 `json:"median"`
	Q1     float64 `json:"q1"`
	Q3     float64 `json:"q3"`
}

// Quantile returns the q-quantile (0 <= q <= 1) of xs by linear interpolation
// between order statistics (NumPy's default, "linear"). It does not modify xs.
// An empty slice gives NaN.
func Quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

// Summarize returns the median and the quartiles of xs.
func Summarize(xs []float64) Summary {
	return Summary{Median: Quantile(xs, 0.5), Q1: Quantile(xs, 0.25), Q3: Quantile(xs, 0.75)}
}

// Verdict values, from the design (docs/performance-campaign.md, rule 6).
const (
	Slower = "slower"
	Tie    = "tie"
	Faster = "faster"
)

// Judge compares the Go side with the reference. Slower when the Go first
// quartile is above the reference third quartile; faster when the Go third
// quartile is below the reference first quartile; a tie when the ranges overlap.
func Judge(goS, ref Summary) string {
	switch {
	case goS.Q1 > ref.Q3:
		return Slower
	case goS.Q3 < ref.Q1:
		return Faster
	}
	return Tie
}
