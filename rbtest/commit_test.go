package rbtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-scikit/refbench"
)

const fakeHead = "0123456789abcdef0123456789abcdef01234567"

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
}

func put(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOut(dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const benchMod = `module example.com/lib/bench

go 1.27

require (
	example.com/lib v0.0.0
	example.com/other v1.0.0
)

replace example.com/lib => ../
`

// repoAt makes a git repository whose module root is dir/sub ("" = the top).
func repoAt(t *testing.T, sub string) (top, root, head string) {
	t.Helper()
	needGit(t)
	top = t.TempDir()
	root = filepath.Join(top, sub)
	put(t, root, "go.mod", "module example.com/lib\n\ngo 1.27\n")
	put(t, root, "lib.go", "package lib\n")
	put(t, root, "bench/go.mod", benchMod)
	put(t, root, "bench/go.sum", "a b c\n")
	put(t, root, "xbench/go.mod", benchMod)
	git(t, top, "init", "-q")
	git(t, top, "add", "-A")
	git(t, top, "commit", "-q", "-m", "init")
	return top, root, strings.TrimSpace(git(t, top, "rev-parse", "HEAD"))
}

func TestResolveCommitRealGit(t *testing.T) {
	bump := strings.Replace(benchMod, "example.com/other v1.0.0", "example.com/other v1.1.0", 1)
	cases := []struct {
		name    string
		sub     string
		mutate  func(t *testing.T, top, root string)
		wantErr string // substring; "" = accepted
	}{
		{name: "clean", mutate: func(*testing.T, string, string) {}},
		{name: "dirty library file", mutate: func(t *testing.T, _, r string) { put(t, r, "lib.go", "package lib // x\n") }, wantErr: "lib.go"},
		{name: "untracked go file", mutate: func(t *testing.T, _, r string) { put(t, r, "new.go", "package lib\n") }, wantErr: "new.go"},
		{name: "rename", mutate: func(t *testing.T, top, r string) { git(t, r, "mv", "lib.go", "lib2.go") }, wantErr: "lib2.go"},
		{name: "xbench/go.mod is not the harness", mutate: func(t *testing.T, _, r string) { put(t, r, "xbench/go.mod", bump) }, wantErr: "xbench/go.mod"},
		{name: "bench/go.mod require bump", mutate: func(t *testing.T, _, r string) { put(t, r, "bench/go.mod", bump) }},
		{name: "bench/go.mod and go.sum bump", mutate: func(t *testing.T, _, r string) {
			put(t, r, "bench/go.mod", bump)
			put(t, r, "bench/go.sum", "a b c\nd e f\n")
		}},
		{name: "bench/go.sum alone", mutate: func(t *testing.T, _, r string) { put(t, r, "bench/go.sum", "x\n") }, wantErr: "bench/go.sum"},
		{name: "replace of the library changed", mutate: func(t *testing.T, _, r string) {
			put(t, r, "bench/go.mod", strings.Replace(benchMod, "=> ../", "=> ../elsewhere", 1))
		}, wantErr: "bench/go.mod"},
		{name: "replace of the library removed", mutate: func(t *testing.T, _, r string) {
			put(t, r, "bench/go.mod", strings.Replace(benchMod, "replace example.com/lib => ../\n", "", 1))
		}, wantErr: "bench/go.mod"},
		{name: "require of the library pinned", mutate: func(t *testing.T, _, r string) {
			put(t, r, "bench/go.mod", strings.Replace(benchMod, "example.com/lib v0.0.0", "example.com/lib v0.9.0", 1))
		}, wantErr: "bench/go.mod"},
		{name: "go directive changed", mutate: func(t *testing.T, _, r string) {
			put(t, r, "bench/go.mod", strings.Replace(benchMod, "go 1.27", "go 1.26", 1))
		}, wantErr: "bench/go.mod"},
		{name: "nested module, harness bump", sub: "sub", mutate: func(t *testing.T, _, r string) { put(t, r, "bench/go.mod", bump) }},
		{name: "nested module, library file", sub: "sub", mutate: func(t *testing.T, _, r string) { put(t, r, "lib.go", "package lib // x\n") }, wantErr: "lib.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			top, root, head := repoAt(t, c.sub)
			c.mutate(t, top, root)
			got, err := resolveCommit(root, head)
			if c.wantErr == "" {
				if err != nil || got != head {
					t.Fatalf("got %q, %v; want %q accepted", got, err, head)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("got %q, %v; want a refusal naming %q", got, err, c.wantErr)
			}
		})
	}
}

func TestResolveCommitMismatchAndPrefix(t *testing.T) {
	_, root, head := repoAt(t, "")
	for _, flag := range []string{head[:7], strings.Repeat("a", 40)} {
		_, err := resolveCommit(root, flag)
		if err == nil || !strings.Contains(err.Error(), head) || !strings.Contains(err.Error(), flag) {
			t.Errorf("flag %q: %v", flag, err)
		}
	}
}

func TestDirtyMarkerTellsTreesApart(t *testing.T) {
	_, root, head := repoAt(t, "")
	mark := func(content string) string {
		put(t, root, "lib.go", content)
		got, err := resolveCommit(root, "")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	a, b, a2 := mark("package lib // a\n"), mark("package lib // b\n"), mark("package lib // a\n")
	if !strings.HasPrefix(a, head+"-dirty-") || len(a) != len(head)+len("-dirty-")+8 {
		t.Fatalf("marker %q", a)
	}
	if a == b || a != a2 {
		t.Fatalf("markers a=%q b=%q a2=%q", a, b, a2)
	}
	put(t, root, "lib.go", "package lib\n")
	if got, _ := resolveCommit(root, ""); got != head {
		t.Fatalf("clean again: %q", got)
	}
}

func TestResolveCommitNoGit(t *testing.T) {
	old := gitLookup
	t.Cleanup(func() { gitLookup = old })
	gitLookup = func(string) (gitState, error) { return gitState{}, os.ErrNotExist }
	if _, err := resolveCommit("r", fakeHead); err == nil || !strings.Contains(err.Error(), "cannot be checked") {
		t.Fatalf("flag without git: %v", err)
	}
	if got, err := resolveCommit("r", ""); err != nil || got != "" {
		t.Fatalf("no flag, no git: %q, %v", got, err)
	}
}

func TestGitEnvIsCleared(t *testing.T) {
	_, root, head := repoAt(t, "")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "nowhere"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	if got, err := resolveCommit(root, head); err != nil || got != head {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestPerfRefusesWrongCommit(t *testing.T) {
	setFlags(t, true, false)
	old := gitLookup
	t.Cleanup(func() { gitLookup = old })
	gitLookup = func(string) (gitState, error) { return gitState{Head: fakeHead}, nil }
	*commitFlag = fakeHead[:7]
	got := outcome(func() { perf(&fakeT{}, repo(t, nil), refbench.Options{}) })
	if !strings.HasPrefix(got, "fatal: -perf.commit") || !strings.Contains(got, fakeHead) {
		t.Fatalf("got %q", got)
	}
}

func TestRecordRefusesOtherCommit(t *testing.T) {
	setFlags(t, false, true)
	old := gitLookup
	t.Cleanup(func() { gitLookup = old })
	gitLookup = func(string) (gitState, error) { return gitState{Head: fakeHead}, nil }
	cfg := repo(t, nil)
	jsonPath, _ := cfg.Paths()
	prev := &refbench.Report{Schema: refbench.SchemaVersion, Status: refbench.StatusMeasured, Commit: strings.Repeat("b", 40), Repo: "r/x"}
	var b strings.Builder
	if err := prev.Write(&b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := outcome(func() { perf(&fakeT{}, cfg, refbench.Options{}) })
	if !strings.Contains(got, "mix commits") {
		t.Fatalf("got %q", got)
	}
}
