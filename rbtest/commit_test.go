package rbtest

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-scikit/refbench"
)

const head = "0123456789abcdef0123456789abcdef01234567"

func fakeGit(t *testing.T, st gitState, err error) {
	t.Helper()
	old := gitLookup
	t.Cleanup(func() { gitLookup = old })
	gitLookup = func(string) (gitState, error) { return st, err }
}

func TestResolveCommit(t *testing.T) {
	noGit := errors.New("git: executable file not found")
	cases := []struct {
		name    string
		st      gitState
		gitErr  error
		flag    string
		want    string
		wantErr []string // substrings; nil = success
	}{
		{name: "match", st: gitState{Head: head}, flag: head, want: head},
		{name: "mismatch", st: gitState{Head: head}, flag: strings.Repeat("a", 40), wantErr: []string{head, strings.Repeat("a", 40)}},
		{name: "prefix only", st: gitState{Head: head}, flag: head[:7], wantErr: []string{head, head[:7]}},
		{name: "dirty library file with flag", st: gitState{Head: head, Dirty: []string{"lib.go", "bench/go.mod"}}, flag: head, wantErr: []string{"lib.go"}},
		{name: "dirty harness only with flag", st: gitState{Head: head, Dirty: []string{"bench/go.mod", "bench/go.sum"}}, flag: head, want: head},
		{name: "no flag clean", st: gitState{Head: head}, want: head},
		{name: "no flag dirty", st: gitState{Head: head, Dirty: []string{"lib.go"}}, want: head + "-dirty"},
		{name: "no flag dirty harness only", st: gitState{Head: head, Dirty: []string{"bench/go.sum"}}, want: head},
		{name: "no git, flag", gitErr: noGit, flag: head, wantErr: []string{"cannot be checked"}},
		{name: "no git, no flag", gitErr: noGit, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeGit(t, c.st, c.gitErr)
			got, err := resolveCommit("root", c.flag)
			if c.wantErr != nil {
				if err == nil {
					t.Fatalf("want refusal, got %q", got)
				}
				for _, s := range c.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error %q lacks %q", err, s)
					}
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestResolveCommitRealGit(t *testing.T) {
	// The default lookup against this very repository, or skipped without git.
	st, err := gitLookup(".")
	if err != nil {
		t.Skipf("no git here: %v", err)
	}
	if len(st.Head) != 40 {
		t.Fatalf("HEAD %q is not a 40-character SHA", st.Head)
	}
}

func TestPerfRefusesWrongCommit(t *testing.T) {
	setFlags(t, true, false)
	fakeGit(t, gitState{Head: head}, nil)
	*commitFlag = head[:7]
	got := outcome(func() { perf(&fakeT{}, repo(t, nil), refbench.Options{}) })
	if !strings.HasPrefix(got, "fatal: -perf.commit") || !strings.Contains(got, head) {
		t.Fatalf("got %q", got)
	}
}
