package reviewer

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
)

// maxLocalPatchBytes caps the patch read from git for one file, so a generated
// or minified file cannot exhaust memory. A larger patch is left unreviewed.
const maxLocalPatchBytes = 32 << 20

// commitSHA matches a full or abbreviated commit id. Anything else, which
// includes a ref name or an option, is never handed to git.
var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// localDiffer produces the patch of a file with git in a local checkout, for
// files the platform would not supply.
type localDiffer struct {
	dir  string
	base string
	head string
}

// newLocalDiffer returns a localDiffer for the repository at dir, or nil when
// there is none: dir is empty or not a repository, a commit is not a plain
// commit id, or the checkout does not hold both commits. Absence is not an
// error; the caller then reports the files as not reviewed.
func newLocalDiffer(ctx context.Context, dir, base, head string) *localDiffer {
	if dir == "" || !commitSHA.MatchString(base) || !commitSHA.MatchString(head) {
		return nil
	}
	l := &localDiffer{dir: dir, base: base, head: head}
	for _, sha := range []string{base, head} {
		if _, err := l.git(ctx, 1<<10, "cat-file", "-e", sha+"^{commit}"); err != nil {
			slog.Debug("local diff unavailable: commit not in the checkout", "commit", sha, "error", err)
			return nil
		}
	}
	return l
}

// patch returns the hunks of the change to a file between the two commits, in
// the shape the platforms' diffs API uses (starting at the first hunk header),
// and whether there were any. A renamed file is named by both of its paths so
// git pairs them. Paths are passed to git as arguments after "--".
func (l *localDiffer) patch(ctx context.Context, oldPath, newPath string) (string, bool) {
	paths := []string{newPath}
	if oldPath != "" && oldPath != newPath {
		paths = []string{oldPath, newPath}
	}
	args := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", l.base + "..." + l.head, "--"}, paths...)
	out, err := l.git(ctx, maxLocalPatchBytes, args...)
	if err != nil {
		slog.Debug("local diff failed", "file", newPath, "error", err)
		return "", false
	}
	text := string(out)
	if !strings.HasPrefix(text, "@@") {
		_, rest, found := strings.Cut(text, "\n@@")
		if !found {
			return "", false // No hunks: unchanged, binary or mode-only.
		}
		text = "@@" + rest
	}
	return text, true
}

// git runs git in the repository and returns at most limit bytes of its
// output; a longer output is an error. No shell is involved. User and system
// configuration cannot start an external diff program or a text conversion
// filter, because the flags of patch disable both.
func (l *localDiffer) git(ctx context.Context, limit int64, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotepath=off"}, args...)...)
	cmd.Dir = l.dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if int64(len(out)) > limit {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errPatchTooLarge
	}
	if readErr != nil {
		_ = cmd.Wait()
		return nil, readErr
	}
	if err := cmd.Wait(); err != nil {
		return nil, &gitError{err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return out, nil
}

type gitError struct {
	err    error
	stderr string
}

func (e *gitError) Error() string { return e.err.Error() + ": " + e.stderr }
func (e *gitError) Unwrap() error { return e.err }

type localDiffError string

func (e localDiffError) Error() string { return string(e) }

const errPatchTooLarge = localDiffError("patch exceeds the size limit")
