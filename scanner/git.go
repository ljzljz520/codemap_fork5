package scanner

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const gitContextWaitDelay = 100 * time.Millisecond

// DiffInfo holds all diff-related data for changed files
type DiffInfo struct {
	Changed   map[string]bool     // all changed files (modified + untracked)
	Untracked map[string]bool     // new/untracked files only
	Stats     map[string]DiffStat // +/- line counts
}

// GitDiffInfo returns comprehensive diff information for the repo while
// honoring cancellation of Git subprocesses.
func GitDiffInfo(ctx context.Context, root, ref string) (*DiffInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info := &DiffInfo{
		Changed:   make(map[string]bool),
		Untracked: make(map[string]bool),
		Stats:     make(map[string]DiffStat),
	}

	// Get modified files vs ref with stats
	cmd := exec.CommandContext(ctx, "git", "diff", "--numstat", ref)
	cmd.WaitDelay = gitContextWaitDelay
	cmd.Dir = root
	// stdout is numstat rows only; read the error text from stderr.
	output, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var detail string
		if exitErr, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		if detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}

	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		// numstat rows are "<added>\t<removed>\t<path>"; reject anything else.
		if len(parts) >= 3 && isNumstatCount(parts[0]) && isNumstatCount(parts[1]) {
			var added, removed int
			if parts[0] != "-" {
				fmt.Sscanf(parts[0], "%d", &added)
			}
			if parts[1] != "-" {
				fmt.Sscanf(parts[1], "%d", &removed)
			}
			filename := strings.Join(parts[2:], " ")
			info.Changed[filename] = true
			info.Stats[filename] = DiffStat{Added: added, Removed: removed}
		}
	}

	// Get untracked files (new files)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd2 := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard")
	cmd2.WaitDelay = gitContextWaitDelay
	cmd2.Dir = root
	output2, err := cmd2.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		output2 = nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output2)), "\n") {
		if line != "" {
			info.Changed[line] = true
			info.Untracked[line] = true
		}
	}

	return info, nil
}

// DropUntracked removes untracked files from the change set. A branch or PR
// comparison must not attribute a reviewer's scratch files to the change
// under review (#154).
func (d *DiffInfo) DropUntracked() {
	for path := range d.Untracked {
		delete(d.Changed, path)
	}
	d.Untracked = make(map[string]bool)
}

// isNumstatCount reports whether s is a numstat count: all digits or "-".
func isNumstatCount(s string) bool {
	if s == "-" {
		return true
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// DiffStat returns +/- line counts for changed files
type DiffStat struct {
	Added   int
	Removed int
}

func GitDiffStats(ctx context.Context, root, ref string) (map[string]DiffStat, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--numstat", ref)
	cmd.WaitDelay = gitContextWaitDelay
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}

	stats := make(map[string]DiffStat)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 3 {
			var added, removed int
			if parts[0] != "-" {
				fmt.Sscanf(parts[0], "%d", &added)
			}
			if parts[1] != "-" {
				fmt.Sscanf(parts[1], "%d", &removed)
			}
			// parts[2] is the filename, but could have spaces - rejoin
			filename := strings.Join(parts[2:], " ")
			stats[filename] = DiffStat{Added: added, Removed: removed}
		}
	}
	return stats, nil
}

// FilterToChanged filters a slice of FileInfo to only include changed files
func FilterToChanged(files []FileInfo, changed map[string]bool) []FileInfo {
	var result []FileInfo
	for _, f := range files {
		if changed[f.Path] || changed[filepath.ToSlash(f.Path)] {
			result = append(result, f)
		}
	}
	return result
}

// FilterToChangedWithInfo filters and annotates files with diff info
func FilterToChangedWithInfo(files []FileInfo, info *DiffInfo) []FileInfo {
	var result []FileInfo
	for _, f := range files {
		path := f.Path
		slashPath := filepath.ToSlash(f.Path)
		if info.Changed[path] || info.Changed[slashPath] {
			// Annotate with diff info
			f.IsNew = info.Untracked[path] || info.Untracked[slashPath]
			if stat, ok := info.Stats[path]; ok {
				f.Added = stat.Added
				f.Removed = stat.Removed
			} else if stat, ok := info.Stats[slashPath]; ok {
				f.Added = stat.Added
				f.Removed = stat.Removed
			}
			result = append(result, f)
		}
	}
	return result
}

// FilterAnalysisToChanged filters FileAnalysis slice to only changed files
func FilterAnalysisToChanged(files []FileAnalysis, changed map[string]bool) []FileAnalysis {
	var result []FileAnalysis
	for _, f := range files {
		if changed[f.Path] || changed[filepath.ToSlash(f.Path)] {
			result = append(result, f)
		}
	}
	return result
}

// Diff impact types and analysis live in impact.go: the plain diff view, the
// MCP get_diff result and blast-radius all build impact from one resolved
// FileGraph, keyed by canonical path rather than file/directory basenames.
