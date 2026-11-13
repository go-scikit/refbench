package refbench

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// skipDirs are the path elements that are not public API (the inventory rule of
// docs/performance-campaign.md section 2.1 in go-scikit/roadmap).
var skipDirs = map[string]bool{
	"internal": true, "cmd": true, "tools": true, "bench": true, "benchmarks": true,
	"examples": true, "testdata": true, "vendor": true,
}

// skipMethods are left out of the public count.
var skipMethods = map[string]bool{
	"Error": true, "String": true, "Format": true, "Unwrap": true, "Is": true,
	"MarshalJSON": true, "UnmarshalJSON": true, "MarshalText": true, "UnmarshalText": true,
	"MarshalBinary": true, "UnmarshalBinary": true,
}

// PublicFuncs lists the exported functions and the exported methods of exported
// types below root, as "Func", "Type.Method", or "dir/sub.Func" for a package
// below the root. Test files, package main and the skipDirs are not read.
func PublicFuncs(root string) ([]string, error) {
	set := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (skipDirs[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if f.Name.Name == "main" {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path)) // path is under root: Rel cannot fail
		prefix := ""
		if rel != "." {
			prefix = filepath.ToSlash(rel) + "."
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if key := funcKey(fn); key != "" {
					set[prefix+key] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func funcKey(fn *ast.FuncDecl) string {
	if !fn.Name.IsExported() {
		return ""
	}
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.ParenExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		}
		break
	}
	id, ok := t.(*ast.Ident)
	if !ok || !id.IsExported() || skipMethods[fn.Name.Name] {
		return ""
	}
	return id.Name + "." + fn.Name.Name
}

// maxExemptLine bounds a line of docs/perf-exempt.md.
const maxExemptLine = 64 << 10

// ParseExempt reads docs/perf-exempt.md. Every list item "- `Name`: reason" or
// "- Name: reason" (the separator may also be a dash) names a function and must
// carry a reason; other lines are prose. A name appearing twice is an error.
func ParseExempt(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, maxExemptLine)
	for n := 1; sc.Scan(); n++ {
		line, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "- ")
		if !ok {
			continue
		}
		var name, rest string
		if t, ok := strings.CutPrefix(line, "`"); ok {
			name, rest, ok = strings.Cut(t, "`")
			if !ok {
				return nil, fmt.Errorf("perf-exempt.md:%d: unterminated backquote", n)
			}
		} else if i := strings.IndexAny(line, " :"); i >= 0 {
			name, rest = line[:i], line[i:]
		} else {
			name = line
		}
		reason := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(rest), ":—–-"))
		switch {
		case name == "":
			return nil, fmt.Errorf("perf-exempt.md:%d: no function name", n)
		case reason == "":
			return nil, fmt.Errorf("perf-exempt.md:%d: %s is exempt without a reason", n, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("perf-exempt.md:%d: %s is listed twice", n, name)
		}
		out[name] = reason
	}
	return out, sc.Err()
}

// Coverage is the result of the inventory gate.
type Coverage struct {
	// Missing: public functions with neither a case nor an exemption.
	Missing []string
	// Unknown: names in a case or in the exemptions that are not public functions.
	Unknown []string
	// Both: functions that have a case and an exemption.
	Both []string
}

// OK reports whether the gate passes.
func (c Coverage) OK() bool { return len(c.Missing)+len(c.Unknown)+len(c.Both) == 0 }

// CheckCoverage applies the gate: every public function is registered by a case
// or named in the exemptions, with a reason, and nothing else is named.
func CheckCoverage(public []string, cases []Case, exempt map[string]string) Coverage {
	pub := map[string]bool{}
	for _, p := range public {
		pub[p] = true
	}
	covered := map[string]bool{}
	var c Coverage
	for _, cs := range cases {
		covered[cs.Func] = true
		if !pub[cs.Func] {
			c.Unknown = append(c.Unknown, cs.Func)
		}
	}
	for name := range exempt {
		if !pub[name] {
			c.Unknown = append(c.Unknown, name)
		} else if covered[name] {
			c.Both = append(c.Both, name)
		}
	}
	for _, p := range public {
		if _, ex := exempt[p]; !covered[p] && !ex {
			c.Missing = append(c.Missing, p)
		}
	}
	sort.Strings(c.Missing)
	sort.Strings(c.Unknown)
	sort.Strings(c.Both)
	return c
}

// CheckAllocs compares the allocation count of every case with the report's:
// an increase is a regression, a decrease must be recorded in perf.json.
func CheckAllocs(rep *Report, cases []Case) []string {
	rows := map[string]Row{}
	for _, r := range rep.Rows {
		rows[r.Name] = r
	}
	var bad []string
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.Name] = true
		row, ok := rows[c.Name]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s: no row in perf.json (record it)", c.Name))
			continue
		}
		switch got := AllocsPerCall(c); {
		case got > row.Allocs:
			bad = append(bad, fmt.Sprintf("%s: %g allocs per call, perf.json has %g (regression)", c.Name, got, row.Allocs))
		case got < row.Allocs:
			bad = append(bad, fmt.Sprintf("%s: %g allocs per call, perf.json has %g (record the improvement)", c.Name, got, row.Allocs))
		}
	}
	for _, r := range rep.Rows {
		if !seen[r.Name] {
			bad = append(bad, fmt.Sprintf("%s: a row in perf.json with no case", r.Name))
		}
	}
	sort.Strings(bad)
	return bad
}

// CIConfig locates a repository's files. Zero paths take the conventional ones.
type CIConfig struct {
	// Root is the repository root, read for the public functions.
	Root string
	// JSON, Table and Exempt default to docs/perf.json, docs/perf-table.md and
	// docs/perf-exempt.md under Root. A missing exemption file means none.
	JSON, Table, Exempt string
	Cases               []Case
}

// openExempt is replaceable by tests: which errors opening a path produces
// differs between operating systems.
var openExempt = os.Open

func (c CIConfig) withDefaults() CIConfig {
	def := func(p *string, name string) {
		if *p == "" {
			*p = filepath.Join(c.Root, "docs", name)
		}
	}
	def(&c.JSON, "perf.json")
	def(&c.Table, "perf-table.md")
	def(&c.Exempt, "perf-exempt.md")
	return c
}

// Paths returns the perf.json and perf-table.md paths, defaults filled in.
func (c CIConfig) Paths() (jsonPath, tablePath string) {
	c = c.withDefaults()
	return c.JSON, c.Table
}

// RunCI is the pull-request check: no Python, no timing. It returns nil when
// the allocations match perf.json, perf-table.md is what perf.json generates and
// every public function has a case or a named exemption; otherwise an error
// listing every problem.
func RunCI(cfg CIConfig) error {
	cfg = cfg.withDefaults()
	if err := Validate(cfg.Cases); err != nil {
		return err
	}
	f, err := os.Open(cfg.JSON)
	if err != nil {
		return err
	}
	defer f.Close()
	rep, err := ReadReport(f)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range CheckAllocs(rep, cfg.Cases) {
		errs = append(errs, errors.New("allocations: "+p))
	}
	for _, p := range CheckRefs(rep, cfg.Cases) {
		errs = append(errs, errors.New("references: "+p))
	}
	got, err := os.ReadFile(cfg.Table)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, Table(rep)) {
		errs = append(errs, fmt.Errorf("%s is not what %s generates (run refbench table)", cfg.Table, cfg.JSON))
	}
	exempt := map[string]string{}
	if ef, err := openExempt(cfg.Exempt); err == nil {
		exempt, err = ParseExempt(ef)
		ef.Close()
		if err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	public, err := PublicFuncs(cfg.Root)
	if err != nil {
		return err
	}
	cov := CheckCoverage(public, cfg.Cases, exempt)
	for _, m := range cov.Missing {
		errs = append(errs, fmt.Errorf("coverage: %s has no case and no exemption", m))
	}
	for _, m := range cov.Unknown {
		errs = append(errs, fmt.Errorf("coverage: %s is named but is not a public function", m))
	}
	for _, m := range cov.Both {
		errs = append(errs, fmt.Errorf("coverage: %s has a case and an exemption", m))
	}
	return errors.Join(errs...)
}
