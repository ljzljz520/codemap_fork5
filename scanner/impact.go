package scanner

import (
	"context"
	"path/filepath"
	"slices"
	"sort"

	"codemap/analysis"
)

// ImpactGranularity names the unit an importer count is measured at.
type ImpactGranularity string

const (
	// ImpactGranularityFile means imports resolve to individual files
	// (TypeScript, Python, ...), so UsedBy counts unique importer files.
	ImpactGranularityFile ImpactGranularity = "file"
	// ImpactGranularityPackage means the language names packages rather than
	// files (Go). UsedBy still counts unique importer files, but every
	// non-test .go file of one package is linked by the same importers.
	ImpactGranularityPackage ImpactGranularity = "package"
)

// ImpactInfo describes one changed file that is used by other files. All
// entries come from a resolved FileGraph, so File is the canonical
// slash-separated path relative to the repo root rather than a basename: two
// same-named files in different directories never collapse into one entry.
type ImpactInfo struct {
	// File is the canonical path of the changed file.
	File string
	// UsedBy is the number of unique files importing File. A file that
	// references File through several import specs (alias + relative,
	// duplicate imports, "from pkg import a, b") contributes once.
	UsedBy int
	// Granularity is the unit UsedBy is measured at.
	Granularity ImpactGranularity `json:",omitempty"`
	// Package is the Go import path when Granularity is ImpactGranularityPackage.
	Package string `json:",omitempty"`
	// Importers are the canonical paths that produce UsedBy. The list is the
	// full unique set even when a renderer truncates what it displays, so
	// output truncation never changes the reported count.
	Importers []string `json:",omitempty"`
}

// ImpactResult is the unified diff-impact answer shared by the plain diff
// view, the MCP get_diff result and blast-radius. It is built from one parsed
// FileGraph so the three entry points cannot disagree.
type ImpactResult struct {
	// Entries are changed files that have at least one importer, ordered by
	// UsedBy descending and File ascending. Files with zero importers are
	// omitted, not reported as zero.
	Entries []ImpactInfo
	// Known reports whether a dependency scan backed the result. When false
	// (ast-grep unavailable, scan bound refused, no graph), Entries is empty
	// because impact is unknown — never because impact is zero.
	Known bool
	// CoverageStatus is the graph's coverage contract: complete, partial, or
	// unavailable.
	CoverageStatus analysis.CoverageStatus `json:",omitempty"`
	// CoverageNotes explains partial/unavailable coverage.
	CoverageNotes []string
}

// AnalyzeImpactFromGraph computes impact for changedFiles from an already
// resolved FileGraph. Counts come from graph edges, which the resolver keyed
// by canonical path: aliases and relative spellings that land on the same
// file are one edge, and external packages that share a name with a local
// file stay external and never count.
func AnalyzeImpactFromGraph(changedFiles []FileInfo, fg *FileGraph) ImpactResult {
	if fg == nil {
		return unknownImpact(nil)
	}
	result := ImpactResult{
		Known:          true,
		Entries:        []ImpactInfo{},
		CoverageStatus: fg.Coverage.Contract().Status,
		CoverageNotes:  slices.Clone(fg.Coverage.Notes),
	}
	if result.CoverageStatus == analysis.CoverageUnavailable {
		result.Known = false
	}

	seen := make(map[string]struct{})
	for _, f := range changedFiles {
		path := canonicalImpactPath(f.Path)
		if path == "" {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}

		importers := uniqueSorted(fg.Importers[path])
		importers = slices.DeleteFunc(importers, func(importer string) bool {
			return canonicalImpactPath(importer) == path
		})
		if len(importers) == 0 {
			continue
		}
		entry := ImpactInfo{
			File:        path,
			UsedBy:      len(importers),
			Granularity: ImpactGranularityFile,
			Importers:   importers,
		}
		// Go imports name packages: the graph fans each package import out
		// to every non-test file of the package, so mark that the count is a
		// package importer count and carry the package's import path.
		if pkg, _, ok := fg.GoPackage(path); ok {
			entry.Granularity = ImpactGranularityPackage
			entry.Package = pkg
		}
		result.Entries = append(result.Entries, entry)
	}

	// Count descending, canonical path ascending. The tie-break is the full
	// path, so same-named files in different directories order deterministically
	// and a display budget always shows the same subset.
	sort.SliceStable(result.Entries, func(i, j int) bool {
		if result.Entries[i].UsedBy != result.Entries[j].UsedBy {
			return result.Entries[i].UsedBy > result.Entries[j].UsedBy
		}
		return result.Entries[i].File < result.Entries[j].File
	})
	return result
}

// AnalyzeImpact scans the repository, builds the FileGraph, and computes
// impact from it, honoring caller cancellation. Callers that already hold a
// scan outcome or graph should use AnalyzeImpactFromGraph instead to avoid
// rescanning. A failed dependency scan is not an error: the returned result
// carries Known=false so callers say "unknown" instead of silently rendering
// zero; only cancellation and other context failures return an error.
func AnalyzeImpact(ctx context.Context, root string, changedFiles []FileInfo, filters Filters) (ImpactResult, error) {
	if err := ctx.Err(); err != nil {
		return ImpactResult{}, err
	}
	if len(changedFiles) == 0 {
		return ImpactResult{Known: true}, nil
	}

	outcome, err := ScanForDeps(ctx, root, filters)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ImpactResult{}, ctxErr
		}
		return unknownImpact(err), nil
	}
	fg, err := BuildFileGraphFromOutcome(ctx, root, outcome, filters)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ImpactResult{}, ctxErr
		}
		return unknownImpact(err), nil
	}
	return AnalyzeImpactFromGraph(changedFiles, fg), ctx.Err()
}

// unknownImpact marks impact as not computable. err, when present, explains
// why the scan was unavailable.
func unknownImpact(err error) ImpactResult {
	note := "no dependency graph is available; impact is unknown, not zero"
	if err != nil {
		note = "dependency scan did not run: " + err.Error() + "; impact is unknown, not zero"
	}
	return ImpactResult{
		Known:          false,
		CoverageStatus: analysis.CoverageUnavailable,
		CoverageNotes:  []string{note},
	}
}

// ApplyImpact stores a graph-derived impact result on a diff project so every
// entry point (plain diff, MCP get_diff, blast-radius) fills the same fields
// the same way. Call it before any display subsetting: ImpactTotal keeps the
// whole-diff count while Impact may later be filtered to the shown files.
func (p *Project) ApplyImpact(result ImpactResult) {
	if p == nil {
		return
	}
	p.Impact = result.Entries
	p.ImpactTotal = len(result.Entries)
	known := result.Known
	p.ImpactKnown = &known
	p.ImpactCoverageStatus = string(result.CoverageStatus)
	if len(result.CoverageNotes) > 0 {
		p.ImpactCoverageNotes = slices.Clone(result.CoverageNotes)
	} else {
		p.ImpactCoverageNotes = nil
	}
}

// FilterImpactEntries keeps the entries whose canonical File is in
// allowedPaths, preserving their order and their full UsedBy counts. It backs
// displays that show only a subset of the diff (for example
// --max-changed-files); callers must keep the unfiltered result's length so
// the omission is reported rather than mistaken for the whole diff.
func FilterImpactEntries(entries []ImpactInfo, allowedPaths map[string]bool) []ImpactInfo {
	if len(entries) == 0 || len(allowedPaths) == 0 {
		return nil
	}
	var kept []ImpactInfo
	for _, entry := range entries {
		if allowedPaths[canonicalImpactPath(entry.File)] {
			kept = append(kept, entry)
		}
	}
	return kept
}

// canonicalImpactPath normalizes a repo-relative path the same way the graph
// keys it: slash-separated and cleaned. It returns "" for paths that name the
// root itself.
func canonicalImpactPath(path string) string {
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if path == "." || path == "" || path == "/" {
		return ""
	}
	return path
}

// uniqueSorted returns the sorted, deduplicated importer set, dropping any
// self reference defensively. The graph already dedupes edges, but graphs
// built through other construction paths are normalized here too so every
// UsedBy is a unique-importer count.
func uniqueSorted(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := slices.Clone(paths)
	sort.Strings(out)
	return slices.Compact(out)
}
