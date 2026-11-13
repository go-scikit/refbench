package refbench

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccuracyWithoutRefsRoundTrips(t *testing.T) {
	// A case with Accuracy and no Refs needs schema 2 (go_accuracy is a schema 2
	// field): the same predicate decides what Finish writes and what ReadReport
	// accepts.
	var log []string
	c := multiCase(&log)
	c.Refs = nil
	c.Go = spinFor(12000)
	c.Accuracy.Budget = 3
	rep, err := Run(context.Background(), []Case{c}, Options{Rounds: 3, MinTime: time.Millisecond, Load: func() (float64, error) { return 1, nil }})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := RecordAllocs([]Case{c}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec.Rows[0].GoAccuracy = &AccResult{Coverage: 1, Points: 1}
	for name, r := range map[string]*Report{"measured": rep, "with accuracy only": rec} {
		if r.Rows[0].GoAccuracy == nil && r.Rows[0].GoBudget == 0 {
			continue // the deferred record before accuracy is scored carries none
		}
		var b bytes.Buffer
		if err := r.Write(&b); err != nil {
			t.Fatal(err)
		}
		if r.Schema != SchemaMultiRef {
			t.Errorf("%s: schema %d, want 2", name, r.Schema)
		}
		back, err := ReadReport(&b)
		if err != nil {
			t.Fatalf("%s: a report this build wrote cannot be read back: %v", name, err)
		}
		if back.Schema != SchemaMultiRef || back.Rows[0].GoAccuracy == nil {
			t.Errorf("%s: read back as schema %d, accuracy %v", name, back.Schema, back.Rows[0].GoAccuracy)
		}
		if tab := string(Table(back)); !strings.Contains(tab, "Against other references") {
			t.Errorf("%s: the accuracy is not in the table:\n%s", name, tab)
		}
	}
	// And a row with none of the fields stays schema 1.
	plain := sampleReport()
	if plain.Rows[0].usesMultiRef() || plain.Schema != SchemaVersion {
		t.Error("a plain row uses schema 2")
	}
}

func TestDisagreeIsBounded(t *testing.T) {
	for _, a := range []AccResult{{Points: 2, Disagree: 3}, {Points: 2, Disagree: -1}} {
		if a.valid() {
			t.Errorf("%+v valid", a)
		}
	}
	if !(AccResult{Points: 2, Disagree: 2}).valid() {
		t.Error("all points disagreeing is valid")
	}
	if s := accCell(&AccResult{MaxULP: 1, MaxULPFin: 1, Coverage: 1, Points: 4, Disagree: 2}); !strings.Contains(s, "2 disagree") {
		t.Errorf("cell = %q", s)
	}
}

func TestRBW1RefusesAnUnverifiedKey(t *testing.T) {
	dir := t.TempDir()
	path, rows, sha, err := WriteF64(dir, "x", []float64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Repeat("0", 64)
	reply, sent := "ok "+sha, 0
	w := fake(t, dir, []string{"f"}, func(line string) string {
		sent++
		if strings.HasPrefix(line, "L ") {
			return reply
		}
		return "ok"
	})
	mustRefuse := func(when string) {
		t.Helper()
		before := sent
		if err := w.Warm("x", "f"); err == nil || !strings.Contains(err.Error(), "no verified load") {
			t.Errorf("%s: Warm: %v", when, err)
		}
		if _, err := w.Time("x", "f", 1); err == nil {
			t.Errorf("%s: Time accepted", when)
		}
		if _, err := w.Timer("x", "f")(1); err == nil {
			t.Errorf("%s: Timer accepted", when)
		}
		if _, _, err := w.Eval("x", "f", filepath.Join(dir, "o")); err == nil {
			t.Errorf("%s: Eval accepted", when)
		}
		if _, err := w.Evaler("x", "f", filepath.Join(dir, "o"), nil)(); err == nil {
			t.Errorf("%s: Evaler accepted", when)
		}
		if sent != before {
			t.Errorf("%s: %d requests reached the worker", when, sent-before)
		}
	}
	// 1. A digest mismatch: nothing may be timed on that key.
	reply = "ok " + bad
	if err := w.Load("x", path, 1, rows, sha); err == nil {
		t.Fatal("mismatch accepted")
	}
	mustRefuse("after a mismatch")
	// 2. A good load verifies the key ...
	reply = "ok " + sha
	if err := w.Load("x", path, 1, rows, sha); err != nil {
		t.Fatal(err)
	}
	if err := w.Warm("x", "f"); err != nil {
		t.Fatalf("verified key refused: %v", err)
	}
	// 3. ... and a reload that fails afterwards un-verifies it, whatever the failure.
	reply = "ok " + bad
	if err := w.Load("x", path, 1, rows, sha); err == nil {
		t.Fatal("mismatch accepted")
	}
	mustRefuse("after a failed reload")
	reply = "ok " + sha
	if err := w.Load("x", path, 1, rows, sha); err != nil {
		t.Fatal(err)
	}
	if err := w.Load("x", "relative", 1, rows, sha); err == nil {
		t.Fatal("bad path accepted")
	}
	mustRefuse("after a reload refused before it was sent")
}

func TestRBW1Boundaries(t *testing.T) {
	dir := t.TempDir()
	var last string
	w := fake(t, dir, []string{"f"}, func(line string) string {
		last = line
		if strings.HasPrefix(line, "L ") {
			return "ok " + strings.Repeat("0", 64)
		}
		return "ok"
	})
	zero := strings.Repeat("0", 64)
	// The request line: exactly 4096 bytes goes, 4097 does not.
	fixed := len("L x  1 1")
	n := RBW1MaxLine - fixed - len(dir) - 1
	exact := filepath.Join(dir, strings.Repeat("a", n))
	if len(exact)+fixed != RBW1MaxLine {
		t.Fatalf("setup: %d", len(exact)+fixed)
	}
	if err := w.Load("x", exact, 1, 1, zero); err != nil || len(last) != RBW1MaxLine {
		t.Errorf("a 4096-byte request: %v (sent %d bytes)", err, len(last))
	}
	last = ""
	if err := w.Load("x", exact+"a", 1, 1, zero); err == nil || last != "" {
		t.Errorf("a 4097-byte request: %v (sent %q)", err, last)
	}
	// The data bound: 256 MiB goes, 8 bytes more does not.
	p := filepath.Join(dir, "a")
	if err := w.Load("x", p, 1, RBW1MaxData/8, zero); err != nil {
		t.Errorf("256 MiB refused: %v", err)
	}
	if err := w.Load("x", p, 1, RBW1MaxData/8+1, zero); err == nil {
		t.Error("256 MiB + 8 bytes accepted")
	}
	if err := w.Load("x", p, 2, RBW1MaxData/16, zero); err != nil {
		t.Errorf("2 columns of 128 MiB: %v", err)
	}
	// A loaded key: the output path of an evaluation must lie in the run directory.
	if err := w.Load("x", p, 1, 1, zero); err != nil {
		t.Fatal(err)
	}
	last = ""
	if _, _, err := w.Eval("x", "f", filepath.Join(filepath.Dir(dir), "out")); err == nil || last != "" {
		t.Errorf("eval outside the run directory: %v (sent %q)", err, last)
	}
}

func TestRoundWithMoreSamplesThanReferencesIsRefused(t *testing.T) {
	rep := synth(10000)
	rep.Rows[0].Rounds[0].RefsNs["x:extra"] = 1 // one sample too many
	var b bytes.Buffer
	if err := rep.Write(&b); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReport(&b); err == nil || !strings.Contains(err.Error(), "one sample per reference") {
		t.Errorf("err = %v", err)
	}
}
