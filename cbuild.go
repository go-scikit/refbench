package refbench

import (
	"context"
	"fmt"
)

// NoFMAFlags are the flags a reference like Shewchuk's predicates.c requires:
// optimised, with no fused multiply-add contraction (its error-free
// transformations forbid it).
var NoFMAFlags = []string{"-O2", "-ffp-contract=off"}

// BuildC compiles a C reference worker from committed sources, so that the
// library never needs cgo: cc flags... -o out sources... The compiler's output
// is in the error.
func BuildC(ctx context.Context, cc, out string, flags, sources []string) error {
	args := append(append(append([]string{}, flags...), "-o", out), sources...)
	if b, err := execCommand(ctx, cc, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w\n%s", cc, err, b)
	}
	return nil
}
