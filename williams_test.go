package refbench

import "testing"

func TestWilliamsOrdersAreBalanced(t *testing.T) {
	for k := 2; k <= 9; k++ {
		orders := williamsOrders(k)
		want := k
		if k%2 == 1 {
			want = 2 * k
		}
		if len(orders) != want {
			t.Fatalf("k=%d: %d orders, want %d", k, len(orders), want)
		}
		pos := map[[2]int]int{}  // (side, position) -> count
		pair := map[[2]int]int{} // (a, b adjacent, a before b) -> count
		for _, o := range orders {
			seen := map[int]bool{}
			for p, s := range o {
				if s < 0 || s >= k || seen[s] {
					t.Fatalf("k=%d: %v is not a permutation", k, o)
				}
				seen[s] = true
				pos[[2]int{s, p}]++
				if p > 0 {
					pair[[2]int{o[p-1], s}]++
				}
			}
		}
		for s := 0; s < k; s++ {
			for p := 0; p < k; p++ {
				if pos[[2]int{s, p}] != len(orders)/k {
					t.Errorf("k=%d: side %d at position %d %d times, want %d", k, s, p, pos[[2]int{s, p}], len(orders)/k)
				}
			}
		}
		// Every ordered pair follows each other equally often: nobody always
		// follows the same side (first-order carryover).
		wantPair := len(orders) * (k - 1) / (k * (k - 1))
		for a := 0; a < k; a++ {
			for b := 0; b < k; b++ {
				if a != b && pair[[2]int{a, b}] != wantPair {
					t.Errorf("k=%d: %d directly before %d %d times, want %d", k, a, b, pair[[2]int{a, b}], wantPair)
				}
			}
		}
	}
	if o := williamsOrders(2); o[0][0] != 0 || o[1][0] != 1 {
		t.Errorf("k=2 is not Go-first then reference-first: %v", o)
	}
	if before([]int{2, 3}, 0, 1) {
		t.Error("before with neither side present")
	}
	if !before([]int{1, 0}, 1, 0) || before([]int{1, 0}, 0, 1) {
		t.Error("before")
	}
}
