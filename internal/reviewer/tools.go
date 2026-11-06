package reviewer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// registerRepoTools registers the read-only repository tools every registry
// starts with. They are bound to the registry's repo root.
func (r *ToolRegistry) registerRepoTools() {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	for _, t := range []Tool{
		{
			Name:        "read_file",
			Description: "Read file contents from the repository.",
			Usage: `Args: {"path": "<relative path>", "start_line": <optional int>, "end_line": <optional int>}
Max 32 KiB returned. Lines are 1-indexed.`,
			InputSchema: ObjectSchema(map[string]any{"path": str, "start_line": integer, "end_line": integer}, "path"),
			Execute:     r.readFile,
		},
		{
			Name:        "find_definition",
			Description: "Find where a symbol is defined in the codebase.",
			Usage: `Args: {"symbol": "<symbol name>"}
Returns up to 10 matches with surrounding context.`,
			InputSchema: ObjectSchema(map[string]any{"symbol": str}, "symbol"),
			Execute:     r.findDefinition,
		},
		{
			Name:        "list_directory",
			Description: "List files in a directory.",
			Usage: `Args: {"path": "<relative path>"}
Returns up to 200 entries.`,
			InputSchema: ObjectSchema(map[string]any{"path": str}, "path"),
			Execute:     r.listDirectory,
		},
	} {
		if err := r.Register(t); err != nil {
			panic(err) // The built-in names are distinct constants.
		}
	}
}

// ToolSchemas returns the prompt description of the built-in tools.
func ToolSchemas() string {
	return NewToolRegistry("").Prompt()
}

func (r *ToolRegistry) resolvePath(relPath string) (string, error) {
	absRepoRoot, err := filepath.Abs(r.repoRoot)
	if err != nil {
		return "", err
	}
	absRepoRoot, err = filepath.EvalSymlinks(absRepoRoot)
	if err != nil {
		return "", err
	}

	targetPath := filepath.Join(absRepoRoot, filepath.Clean(filepath.FromSlash(relPath)))

	resolved, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			// If file doesn't exist, check base path
			if !strings.HasPrefix(targetPath, absRepoRoot+string(filepath.Separator)) && targetPath != absRepoRoot {
				return "", fmt.Errorf("path escapes repository root")
			}
			return targetPath, nil
		}
		return "", err
	}

	if !strings.HasPrefix(resolved, absRepoRoot+string(filepath.Separator)) && resolved != absRepoRoot {
		return "", fmt.Errorf("path escapes repository root")
	}

	return resolved, nil
}

func (r *ToolRegistry) readFile(_ context.Context, args map[string]any) (string, error) {
	path, ok := args["path"].(string)
	if !ok {
		return "", errors.New("missing or invalid 'path' argument")
	}

	resolved, err := r.resolvePath(path)
	if err != nil {
		return "", err
	}

	f, err := os.Open(resolved)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %v", err)
	}
	defer f.Close() //nolint:errcheck

	const maxRead = 32768 + 64 // extra margin for UTF-8 boundary
	buf := make([]byte, maxRead)
	n, _ := io.ReadFull(f, buf) //nolint:errcheck
	content := buf[:n]

	// Ensure we end on a UTF-8 boundary.
	truncated := n >= maxRead
	if truncated {
		for len(content) > 0 && !utf8.Valid(content) {
			content = content[:len(content)-1]
		}
		if len(content) > 32768 {
			content = content[:32768]
		}
	}

	lines := strings.Split(string(content), "\n")

	startLine := 1
	if sl, ok := args["start_line"].(float64); ok {
		startLine = int(sl)
	} else if sl, ok := args["start_line"].(int); ok {
		startLine = sl
	}

	endLine := len(lines)
	if el, ok := args["end_line"].(float64); ok {
		endLine = int(el)
	} else if el, ok := args["end_line"].(int); ok {
		endLine = el
	}

	if startLine < 1 {
		startLine = 1
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > endLine {
		return "", errors.New("start_line is greater than end_line")
	}

	selected := lines[startLine-1 : endLine]
	output := strings.Join(selected, "\n")
	if truncated {
		output += "\n[... truncated at 32 KiB ...]"
	}
	return output, nil
}

func isDefinition(line string, symbol string) bool {
	line = strings.TrimSpace(line)
	patterns := []string{
		"func " + symbol,
		"type " + symbol,
		"var " + symbol,
		"const " + symbol,
		"class " + symbol,
		"def " + symbol,
		"function " + symbol,
	}
	for _, p := range patterns {
		if strings.HasPrefix(line, p) || strings.Contains(line, " "+p) {
			return true
		}
	}
	if strings.HasPrefix(line, "func (") && strings.Contains(line, ") "+symbol) {
		return true
	}
	return false
}

func (r *ToolRegistry) findDefinition(ctx context.Context, args map[string]any) (string, error) {
	symbol, ok := args["symbol"].(string)
	if !ok {
		return "", errors.New("missing or invalid 'symbol' argument")
	}

	var matches []string
	filesScanned := 0

	absRoot, err := filepath.Abs(r.repoRoot)
	if err != nil {
		return "", err
	}
	absRoot, err = filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", err
	}

	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		// Stop promptly on context cancellation.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if filesScanned >= 5000 {
			return filepath.SkipAll
		}

		if len(matches) >= 10 {
			return filepath.SkipAll
		}

		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		ext := filepath.Ext(path)
		switch ext {
		case ".go", ".py", ".js", ".ts", ".java":
		default:
			return nil
		}

		filesScanned++
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		if !utf8.Valid(content) {
			return nil
		}

		relPath, _ := filepath.Rel(absRoot, path)
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if isDefinition(line, symbol) {
				matches = append(matches, fmt.Sprintf("%s:%d: %s", relPath, i+1, strings.TrimSpace(line)))
				if len(matches) >= 10 {
					break
				}
			}
		}

		return nil
	})

	if err != nil {
		return "", fmt.Errorf("error walking directory: %v", err)
	}

	if len(matches) == 0 {
		return "No definitions found.", nil
	}

	return strings.Join(matches, "\n"), nil
}

func (r *ToolRegistry) listDirectory(_ context.Context, args map[string]any) (string, error) {
	path, ok := args["path"].(string)
	if !ok {
		return "", errors.New("missing or invalid 'path' argument")
	}

	resolved, err := r.resolvePath(path)
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return "", fmt.Errorf("failed to read directory: %v", err)
	}

	var output []string
	for i, entry := range entries {
		if i >= 200 {
			output = append(output, "... (truncated after 200 entries)")
			break
		}
		indicator := ""
		if entry.IsDir() {
			indicator = "/"
		}
		output = append(output, entry.Name()+indicator)
	}

	return strings.Join(output, "\n"), nil
}
