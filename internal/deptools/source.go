package deptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/OpticDiff/code-reviewer/internal/textsafe"
)

const (
	maxSourceRead    = 32 << 10
	maxSourceEntries = 200
	maxSourcePathLen = 512
)

func readDependencySourceSpec(opts Options) Spec {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	return Spec{
		Name:        ToolReadDependencySource,
		Description: "Read a file of an installed dependency: typings (.d.ts) and sources under node_modules, or a module in the Go module cache.",
		Usage: `Args: {"package": "<npm name or Go module path>", "path": "<optional path inside the package>", "start_line": <optional int>, "end_line": <optional int>}
Without a path, lists the package's top-level files. A directory path lists it. Max 32 KiB returned; up to 200 directory entries.
Answers "not installed" when the checkout has no copy of the package; that says nothing about whether the package or its API exists.`,
		InputSchema: objectSchema(map[string]any{"package": str, "path": str, "start_line": integer, "end_line": integer}, "package"),
		Untrusted:   true,
		Execute: func(ctx context.Context, args map[string]any) (string, error) {
			pkg, ok := stringArg(args, "package")
			if !ok || pkg == "" {
				return "", errors.New("missing or invalid 'package' argument")
			}
			rel, _ := stringArg(args, "path")
			return readDependencySource(ctx, opts, pkg, rel, intArg(args, "start_line"), intArg(args, "end_line"))
		},
	}
}

func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func readDependencySource(ctx context.Context, opts Options, pkg, rel string, start, end int) (string, error) {
	if !genericName.MatchString(pkg) || hasDotSegment(pkg) {
		return "", errors.New("invalid package name: use letters, digits and . _ - / @ : + ~ only, at most 214 characters")
	}
	if len(rel) > maxSourcePathLen || strings.ContainsRune(rel, 0) {
		return "", errors.New("invalid path")
	}
	dir, version, ok := locateInstalled(ctx, opts, pkg)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("not installed: %q has no copy under node_modules or in the Go module cache of this checkout. "+
			"This says nothing about whether the package or its API exists.", pkg), nil
	}
	target, err := confinedPath(dir, rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("no such path %q in the installed copy of %q", rel, pkg), nil
		}
		return "", fmt.Errorf("failed to read: %v", err)
	}
	if info.IsDir() {
		listing, err := listDir(target)
		if err != nil {
			return "", err
		}
		header := ""
		if version != "" && rel == "" {
			header = "installed version: " + version + "\n"
		}
		return header + listing, nil
	}
	return readSourceFile(target, start, end)
}

// locateInstalled finds the directory holding the installed package: first
// node_modules/<pkg> in the checkout, then the Go module cache at the version
// the checkout's go.mod requires. The returned directory is fully resolved.
func locateInstalled(ctx context.Context, opts Options, pkg string) (dir, version string, ok bool) {
	if npmName.MatchString(pkg) {
		candidate := filepath.Join(opts.RepoRoot, "node_modules", filepath.FromSlash(pkg))
		if real, err := resolveWithin(opts.RepoRoot, candidate); err == nil {
			if st, err := os.Stat(real); err == nil && st.IsDir() {
				return real, npmInstalledVersion(real), true
			}
		}
	}
	if goModule.MatchString(pkg) {
		ver := goRequiredVersion(ctx, opts.RepoRoot, pkg)
		cache := goModCache(ctx, opts)
		if ver != "" && cache != "" {
			candidate := filepath.Join(cache, filepath.FromSlash(escapeGoPath(pkg))+"@"+escapeGoPath(ver))
			if real, err := resolveWithin(cache, candidate); err == nil {
				if st, err := os.Stat(real); err == nil && st.IsDir() {
					return real, ver, true
				}
			}
		}
	}
	return "", "", false
}

// escapeGoPath applies the module cache's case encoding: an upper-case letter
// becomes "!" and its lower-case form.
func escapeGoPath(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			sb.WriteByte('!')
			sb.WriteRune(unicode.ToLower(r))
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func npmInstalledVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var m struct{ Version string }
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return sanitizeVersion(m.Version)
}

func goRequiredVersion(ctx context.Context, root, pkg string) string {
	files, err := findPinFiles(ctx, root)
	if err != nil {
		return ""
	}
	for _, rel := range files {
		if ctx.Err() != nil {
			return ""
		}
		if filepath.Base(rel) != "go.mod" {
			continue
		}
		data, err := readCapped(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		for _, p := range parseGoMod(data, pkg) {
			if p.kind == "locked" && !strings.HasPrefix(p.version, "replaced") {
				return p.version
			}
		}
	}
	return ""
}

// goModCache returns the module cache directory: the option, then GOMODCACHE,
// then `go env GOMODCACHE` run outside the repository with toolchain switching off.
func goModCache(ctx context.Context, opts Options) string {
	if opts.GoModCache != "" {
		return opts.GoModCache
	}
	if v := os.Getenv("GOMODCACHE"); v != "" {
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "env", "GOMODCACHE")
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveWithin resolves symlinks of path and requires the result to lie
// inside the resolved root.
func resolveWithin(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if absRoot, err = filepath.EvalSymlinks(absRoot); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !within(absRoot, resolved) {
		return "", errors.New("path escapes the allowed root")
	}
	return resolved, nil
}

func within(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// confinedPath joins rel onto the resolved directory dir and returns the
// resolved target, refusing absolute paths, ".." segments and symlinks that
// leave dir. A path that does not exist is returned unresolved once its
// lexical form is confined.
func confinedPath(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", errors.New("path escapes the package directory")
	}
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the package directory")
	}
	target := filepath.Join(dir, cleaned)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			return target, nil
		}
		return "", err
	}
	if !within(dir, resolved) {
		return "", errors.New("path escapes the package directory")
	}
	return resolved, nil
}

func listDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("failed to list directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := textsafe.OneLine(e.Name(), maxSourcePathLen)
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	truncated := len(names) > maxSourceEntries
	if truncated {
		names = names[:maxSourceEntries]
	}
	out := strings.Join(names, "\n")
	if truncated {
		out += fmt.Sprintf("\n[... truncated at %d entries ...]", maxSourceEntries)
	}
	return out, nil
}

func readSourceFile(path string, startLine, endLine int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %v", err)
	}
	defer f.Close() //nolint:errcheck
	buf := make([]byte, maxSourceRead+64)
	n, _ := io.ReadFull(f, buf) //nolint:errcheck
	content := buf[:n]
	if strings.ContainsRune(string(content), 0) {
		return "", errors.New("binary file")
	}
	truncated := n >= len(buf)
	if truncated {
		for len(content) > 0 && !utf8.Valid(content) {
			content = content[:len(content)-1]
		}
		if len(content) > maxSourceRead {
			content = content[:maxSourceRead]
		}
	}
	lines := strings.Split(string(content), "\n")
	if startLine < 1 {
		startLine = 1
	}
	if endLine < 1 || endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > endLine {
		return "", errors.New("start_line is greater than end_line")
	}
	out := strings.Join(lines[startLine-1:endLine], "\n")
	if truncated {
		out += "\n[... truncated at 32 KiB ...]"
	}
	return out, nil
}
