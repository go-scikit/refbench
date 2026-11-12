package rbtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	harnessMod = "bench/go.mod"
	harnessSum = "bench/go.sum"
	gitTimeout = 30 * time.Second
)

// gitState is what git says about the tree being measured. Dirty paths are
// relative to the module root.
type gitState struct {
	Head      string   // the 40-character SHA of HEAD
	Dirty     []string // paths git reports as changed, renamed, deleted or untracked
	GoModDiff string   // `git diff HEAD -U1000000 -- bench/go.mod`
	HashAll   string   // digest of every change against HEAD (tracked diff and untracked files)
	HashLib   string   // the same without bench/go.mod and bench/go.sum
}

// gitLookup reads the commit and the changes of the tree at root; tests
// replace it.
var gitLookup = realGitLookup

func realGitLookup(root string) (gitState, error) {
	diff := []string{"diff", "HEAD", "--binary", "--ignore-submodules=none", "--", "."}
	outs, err := gitAll(root, [][]string{
		{"rev-parse", "HEAD"},
		{"rev-parse", "--show-prefix"},
		{"-c", "status.showUntrackedFiles=all", "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=none"},
		{"diff", "HEAD", "-U1000000", "--", harnessMod},
		diff,
		append(slices.Clone(diff), ":(exclude)"+harnessMod, ":(exclude)"+harnessSum),
	})
	if err != nil {
		return gitState{}, err
	}
	prefix := strings.TrimSpace(outs[1])
	st := gitState{Head: strings.TrimSpace(outs[0]), GoModDiff: outs[3]}
	var untracked []string
	entries := strings.Split(outs[2], "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		xy, p := e[:2], e[3:]
		st.Dirty = append(st.Dirty, underRoot(prefix, p))
		if xy == "??" {
			untracked = append(untracked, underRoot(prefix, p))
		}
		if strings.ContainsAny(xy, "RC") && i+1 < len(entries) {
			i++
			st.Dirty = append(st.Dirty, underRoot(prefix, entries[i]))
		}
	}
	st.HashAll = digest(root, outs[4], untracked, false)
	st.HashLib = digest(root, outs[5], untracked, true)
	return st, nil
}

// digest hashes a tracked diff and the content of the untracked files.
func digest(root, diff string, untracked []string, skipHarness bool) string {
	h := sha256.New()
	h.Write([]byte(diff))
	for _, rel := range untracked {
		if skipHarness && (rel == harnessMod || rel == harnessSum) {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		fmt.Fprintf(h, "\x00%s\x00", rel)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// underRoot maps a path relative to the repository's top to one relative to
// the module root (prefix is `git rev-parse --show-prefix`, "" or "sub/");
// a path outside the root is kept as git reports it.
func underRoot(prefix, p string) string {
	if rest, ok := strings.CutPrefix(p, prefix); ok {
		return rest
	}
	return p
}

// gitAll runs each git command in turn and returns the outputs.
func gitAll(root string, cmds [][]string) ([]string, error) {
	outs := make([]string, 0, len(cmds))
	for _, args := range cmds {
		out, err := gitOut(root, args...)
		if err != nil {
			return nil, err
		}
		outs = append(outs, out)
	}
	return outs, nil
}

func gitOut(root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR":
		default:
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// goModAccepted reports whether a diff of bench/go.mod against HEAD (with
// full context) changes nothing but `require` lines of modules other than
// lib, and keeps the `replace lib => ...` line present and unchanged: a
// changed replace or a pinned require of the library would measure other
// code under a clean HEAD.
func goModAccepted(diff, lib string) bool {
	state, replaced, changed := "", false, false
	for _, line := range strings.Split(diff, "\n") {
		if line == "" || strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "@@") ||
			strings.HasPrefix(line, "\\") {
			continue
		}
		mark := line[:1]
		if !strings.Contains(" +-", mark) {
			return false // new file, deleted file, rename...
		}
		body, _, _ := strings.Cut(line[1:], "//")
		f := strings.Fields(body)
		if len(f) == 0 {
			continue
		}
		isChange := mark != " "
		changed = changed || isChange
		isLibReplace := (f[0] == "replace" && len(f) > 1 && f[1] == lib || state == "replace" && f[0] == lib) && slices.Contains(f, "=>")
		switch {
		case len(f) == 2 && f[1] == "(":
			state = f[0]
			if isChange {
				return false
			}
		case f[0] == ")":
			state = ""
			if isChange {
				return false
			}
		case isLibReplace:
			if isChange {
				return false
			}
			replaced = true
		case f[0] == "require" && len(f) > 2 && f[1] != lib, state == "require" && f[0] != lib:
			// a require of another module: a change is accepted
		default:
			if isChange {
				return false
			}
		}
	}
	return changed && replaced
}

// resolveCommit returns the commit to record. With flag set, it must equal
// the 40-character HEAD of the tree at root and the tree must be clean,
// except for bench/go.mod and bench/go.sum when go.mod changes only requires
// of other modules (see goModAccepted). Without the flag, HEAD is filled in,
// suffixed "-dirty-<8 hex of the changes' digest>" when the tree is dirty;
// with no git and no flag, the commit stays empty.
func resolveCommit(root, flag string) (string, error) {
	st, err := gitLookup(root)
	if err != nil {
		if flag != "" {
			return "", fmt.Errorf("-perf.commit %q cannot be checked: %w", flag, err)
		}
		return "", nil
	}
	if flag != "" && flag != st.Head {
		return "", fmt.Errorf("-perf.commit is %q, but HEAD of %s is %q: give the full 40-character SHA of the tree measured (git rev-parse HEAD), or omit the flag", flag, root, st.Head)
	}
	var dirty []string
	goModDirty, sumDirty, harnessOK := false, false, false
	for _, p := range st.Dirty {
		switch s := filepath.ToSlash(p); {
		case s == harnessMod:
			goModDirty = true
		case s == harnessSum:
			sumDirty = true
		default:
			dirty = append(dirty, p)
		}
	}
	if goModDirty {
		lib, err := modulePath(filepath.Join(root, "go.mod"))
		if harnessOK = err == nil && goModAccepted(st.GoModDiff, lib); !harnessOK {
			dirty = append(dirty, harnessMod+" (more than requires of other modules changed, or the replace of the library moved)")
		}
	}
	if sumDirty && !goModDirty {
		dirty = append(dirty, harnessSum+" (without an accepted "+harnessMod+")")
	}
	if len(dirty) == 0 {
		return st.Head, nil
	}
	if flag != "" {
		return "", fmt.Errorf("-perf.commit %q given but the tree at %s is dirty, so a table would claim a commit it did not measure; dirty paths: %s", flag, root, strings.Join(dirty, ", "))
	}
	h := st.HashAll
	if harnessOK {
		h = st.HashLib
	}
	return st.Head + "-dirty-" + h[:8], nil
}
