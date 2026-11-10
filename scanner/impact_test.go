package scanner

import (
	"testing"

	"codemap/analysis"
)

func requireAstGrep(t *testing.T) {
	t.Helper()
	if !NewAstGrepAnalyzer().Available() {
		t.Skip("ast-grep not available")
	}
}

func impactByPath(result ImpactResult) map[string]ImpactInfo {
	byPath := make(map[string]ImpactInfo, len(result.Entries))
	for _, entry := range result.Entries {
		byPath[entry.File] = entry
	}
	return byPath
}

// Two same-named files in different directories used to collapse into one
// basename key, with the later changed file stealing the entry. The graph
// keys edges by canonical path, so each gets its own entry and importer set.
func TestImpactSameNameFilesInDifferentDirs(t *testing.T) {
	requireAstGrep(t)
	graph := falseEdgeGraph(t, map[string]string{
		"src/auth/util.ts":         "export const auth = 1;\n",
		"src/auth/client.ts":       "import { auth } from './util';\nconsole.log(auth);\n",
		"src/payments/util.ts":     "export const pay = 1;\n",
		"src/payments/checkout.ts": "import { pay } from './util';\nconsole.log(pay);\n",
		"src/payments/refund.ts":   "import { pay } from './util';\nconsole.log(pay);\n",
	})
	changed := []FileInfo{{Path: "src/auth/util.ts"}, {Path: "src/payments/util.ts"}}
	result := AnalyzeImpactFromGraph(changed, graph)
	if !result.Known {
		t.Fatalf("impact unknown: %+v", result.CoverageNotes)
	}
	byPath := impactByPath(result)
	if len(byPath) != 2 {
		t.Fatalf("entries = %+v, want exactly the two canonical paths", result.Entries)
	}
	auth := byPath["src/auth/util.ts"]
	if auth.UsedBy != 1 || len(auth.Importers) != 1 || auth.Importers[0] != "src/auth/client.ts" {
		t.Fatalf("auth/util.ts impact = %+v, want only src/auth/client.ts", auth)
	}
	pay := byPath["src/payments/util.ts"]
	if pay.UsedBy != 2 {
		t.Fatalf("payments/util.ts impact = %+v, want 2 unique importers", pay)
	}
	// Each importer is canonical; an auth-side importer must never appear on
	// the payments entry even though both files are named util.ts.
	for _, importer := range pay.Importers {
		if importer == "src/auth/client.ts" {
			t.Fatalf("cross-directory basename collision: %+v", pay.Importers)
		}
	}
}

// An alias specifier and a relative specifier that resolve to the same file
// are one edge: duplicate references from one importer count once.
func TestImpactAliasAndRelativeAreOneUniqueImporter(t *testing.T) {
	requireAstGrep(t)
	graph := falseEdgeGraph(t, map[string]string{
		"tsconfig.json":    `{"compilerOptions":{"baseUrl":".","paths":{"@app/*":["src/*"]}}}`,
		"src/lib/thing.ts": "export const thing = 1;\n",
		"src/consumer.ts":  "import { a } from '@app/lib/thing';\nimport { b } from './lib/thing';\nconsole.log(a, b);\n",
	})
	result := AnalyzeImpactFromGraph([]FileInfo{{Path: "src/lib/thing.ts"}}, graph)
	if len(result.Entries) != 1 {
		t.Fatalf("entries = %+v, want one", result.Entries)
	}
	entry := result.Entries[0]
	if entry.File != "src/lib/thing.ts" || entry.UsedBy != 1 {
		t.Fatalf("entry = %+v, want canonical path with one unique importer", entry)
	}
	if len(entry.Importers) != 1 || entry.Importers[0] != "src/consumer.ts" {
		t.Fatalf("importers = %+v, want [src/consumer.ts]", entry.Importers)
	}
}

// An external package that shares a name with a local file ("react" vs
// src/react.ts) must never be matched to the local file: only the relative
// import counts.
func TestImpactExternalSameNamePackageDoesNotCount(t *testing.T) {
	requireAstGrep(t)
	graph := falseEdgeGraph(t, map[string]string{
		"package.json": `{"name":"app","dependencies":{"react":"18.0.0"}}`,
		"src/react.ts": "export const local = 1;\n",
		"src/app.ts":   "import external from 'react';\nimport local from './react';\nconsole.log(external, local);\n",
	})
	assertImports(t, graph, "src/app.ts", "src/react.ts")
	result := AnalyzeImpactFromGraph([]FileInfo{{Path: "src/react.ts"}}, graph)
	if len(result.Entries) != 1 {
		t.Fatalf("entries = %+v, want one local entry", result.Entries)
	}
	entry := result.Entries[0]
	if entry.File != "src/react.ts" || entry.UsedBy != 1 {
		t.Fatalf("entry = %+v, want src/react.ts used by exactly one importer (the external react import must not count)", entry)
	}
}

// Go imports name packages: the graph fans an import out to every non-test
// file of the package, so changing two files in one package gives two
// package-granularity entries with identical unique importer sets.
func TestImpactGoMultiFilePackageGranularity(t *testing.T) {
	requireAstGrep(t)
	graph := falseEdgeGraph(t, map[string]string{
		"go.mod":          "module example.com/demo\n\ngo 1.21\n",
		"multi/a.go":      "package multi\n\nfunc A() int { return 1 }\n",
		"multi/b.go":      "package multi\n\nfunc B() int { return 2 }\n",
		"multi/a_test.go": "package multi\n\nimport \"testing\"\nfunc TestA(t *testing.T) { A() }\n",
		"app/main.go":     "package main\n\nimport \"example.com/demo/multi\"\n\nfunc main() { _ = multi.A(); _ = multi.B() }\n",
	})
	result := AnalyzeImpactFromGraph([]FileInfo{{Path: "multi/a.go"}, {Path: "multi/b.go"}, {Path: "multi/a_test.go"}}, graph)
	byPath := impactByPath(result)
	for _, path := range []string{"multi/a.go", "multi/b.go"} {
		entry, ok := byPath[path]
		if !ok {
			t.Fatalf("missing entry for %s: %+v", path, result.Entries)
		}
		if entry.Granularity != ImpactGranularityPackage || entry.Package != "example.com/demo/multi" {
			t.Fatalf("%s = %+v, want package granularity for example.com/demo/multi", path, entry)
		}
		if entry.UsedBy != 1 || len(entry.Importers) != 1 || entry.Importers[0] != "app/main.go" {
			t.Fatalf("%s importers = %+v, want only app/main.go once", path, entry.Importers)
		}
	}
	// A changed test file is never an import target: no entry, not an entry
	// that borrows the package's importers by directory matching.
	if _, ok := byPath["multi/a_test.go"]; ok {
		t.Fatalf("_test.go change must not report package importers: %+v", result.Entries)
	}
}

// A graph whose scan was unavailable marks the whole impact result unknown
// even when it happens to contain edges.
func TestImpactUnavailableGraphCoverageIsUnknown(t *testing.T) {
	fg := &FileGraph{
		Importers: map[string][]string{"src/a.ts": {"src/user.ts"}},
		Coverage: GraphCoverage{Sources: []analysis.Source{
			{Name: "ast-grep", Status: analysis.SourceUnavailable},
		}},
	}
	result := AnalyzeImpactFromGraph([]FileInfo{{Path: "src/a.ts"}}, fg)
	if result.Known {
		t.Fatalf("known = true, want false for unavailable scan")
	}
	if result.CoverageStatus != analysis.CoverageUnavailable {
		t.Fatalf("status = %q, want unavailable", result.CoverageStatus)
	}
}

// Display subsetting filters by exact canonical path and leaves the dropped
// entries' totals recoverable via the full result length.
func TestFilterImpactEntriesMatchesCanonicalPaths(t *testing.T) {
	entries := []ImpactInfo{
		{File: "src/auth/util.ts", UsedBy: 3},
		{File: "src/payments/util.ts", UsedBy: 1},
	}
	shown := map[string]bool{"src/auth/util.ts": true}
	kept := FilterImpactEntries(entries, shown)
	if len(kept) != 1 || kept[0].File != "src/auth/util.ts" || kept[0].UsedBy != 3 {
		t.Fatalf("kept = %+v, want only src/auth/util.ts with its full count", kept)
	}
	// A basename match must NOT pass: that was the original collision bug.
	if got := FilterImpactEntries(entries, map[string]bool{"util.ts": true}); got != nil {
		t.Fatalf("basename matching must keep nothing, got %+v", got)
	}
}

// Repeated changed paths produce one entry each, not duplicated counts.
func TestImpactDuplicateChangedPathsCollapse(t *testing.T) {
	fg := &FileGraph{
		Importers: map[string][]string{"a.ts": {"b.ts", "b.ts", "c.ts"}},
		Coverage: GraphCoverage{Sources: []analysis.Source{
			{Name: "ast-grep", Status: ScanSourceAuthoritative, Languages: astGrepLanguages},
		}},
	}
	result := AnalyzeImpactFromGraph([]FileInfo{{Path: "a.ts"}, {Path: "./a.ts"}}, fg)
	if len(result.Entries) != 1 {
		t.Fatalf("entries = %+v, want one", result.Entries)
	}
	if result.Entries[0].UsedBy != 2 {
		t.Fatalf("usedBy = %d, want 2 unique importers after dedupe", result.Entries[0].UsedBy)
	}
}
