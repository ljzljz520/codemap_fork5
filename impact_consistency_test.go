package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"codemap/cmd"
	"codemap/config"
	"codemap/scanner"
)

// makeSameNameImpactRepo builds a TS repo exercising every collision class:
// same-named files in different directories, alias + relative duplicate
// references, and an external package with the same name as a local file.
func makeSameNameImpactRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"package.json":             `{"name":"app","dependencies":{"react":"18.0.0"}}`,
		"tsconfig.json":            `{"compilerOptions":{"baseUrl":".","paths":{"@app/*":["src/*"]}}}`,
		"src/auth/util.ts":         "export const auth = 1;\n",
		"src/auth/client.ts":       "import { auth } from './util';\nconsole.log(auth);\n",
		"src/payments/util.ts":     "export const pay = 1;\n",
		"src/payments/checkout.ts": "import { pay } from './util';\nconsole.log(pay);\n",
		"src/payments/refund.ts":   "import { pay } from './util';\nconsole.log(pay);\n",
		"src/lib/thing.ts":         "export const thing = 1;\n",
		"src/consumer.ts":          "import { a } from '@app/lib/thing';\nimport { b } from './lib/thing';\nconsole.log(a, b);\n",
		"src/react.ts":             "export const local = 1;\n",
		"src/app.ts":               "import external from 'react';\nimport local from './react';\nconsole.log(external, local);\n",
	}
	for relPath, content := range files {
		full := filepath.Join(root, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitMainTestCmd(t, root, "init")
	runGitMainTestCmd(t, root, "add", ".")
	runGitMainTestCmd(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	runGitMainTestCmd(t, root, "branch", "-M", "main")

	// Modify the four targets vs main; importers stay unchanged.
	changes := map[string]string{
		"src/auth/util.ts":     "export const auth = 2;\n",
		"src/payments/util.ts": "export const pay = 2;\n",
		"src/lib/thing.ts":     "export const thing = 2;\n",
		"src/react.ts":         "export const local = 2;\n",
	}
	for relPath, content := range changes {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(relPath)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func impactCounts(entries []scanner.ImpactInfo) map[string]int {
	counts := make(map[string]int, len(entries))
	for _, entry := range entries {
		counts[entry.File] = entry.UsedBy
	}
	return counts
}

func changedFilesForRepo(t *testing.T, root string) []scanner.FileInfo {
	t.Helper()
	ctx := context.Background()
	diffInfo, err := scanner.GitDiffInfo(ctx, root, "main")
	if err != nil {
		t.Fatalf("git diff: %v", err)
	}
	all, err := scanner.ScanFiles(ctx, root, scanner.NewGitIgnoreCache(root), nil, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return scanner.FilterToChangedWithInfo(all, diffInfo)
}

// The plain/MCP graph wrapper and the blast-radius graph must attribute the
// same canonical paths and unique-importer counts, including every collision
// class from makeSameNameImpactRepo.
func TestImpactConsistencyAcrossGraphEntries(t *testing.T) {
	requireBlastRadiusTools(t)
	root := makeSameNameImpactRepo(t)
	cmd.EnsureProjectConfig(root)
	cfg := config.Load(root)
	filters := scanner.Filters{Only: cfg.Only, Exclude: cfg.Exclude, MaxFiles: cfg.MaxFiles}
	changed := changedFilesForRepo(t, root)

	want := map[string]int{
		"src/auth/util.ts":     1,
		"src/payments/util.ts": 2,
		"src/lib/thing.ts":     1, // alias + relative spellings = one importer
		"src/react.ts":         1, // external "react" must not count
	}

	direct, err := scanner.AnalyzeImpact(context.Background(), root, changed, filters)
	if err != nil {
		t.Fatalf("AnalyzeImpact: %v", err)
	}
	if !direct.Known {
		t.Fatalf("direct impact unknown: %v", direct.CoverageNotes)
	}
	if got := impactCounts(direct.Entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("direct impact counts = %v, want %v", got, want)
	}
	// Every entry is file-granularity TS and carries its full importer set.
	for _, entry := range direct.Entries {
		if entry.Granularity != scanner.ImpactGranularityFile {
			t.Fatalf("%s granularity = %s, want file", entry.File, entry.Granularity)
		}
		if len(entry.Importers) != entry.UsedBy {
			t.Fatalf("%s importers %v disagree with UsedBy %d", entry.File, entry.Importers, entry.UsedBy)
		}
	}

	bundle, err := buildBlastRadiusBundle(root, "main", defaultBlastRadiusLimits())
	if err != nil {
		t.Fatalf("buildBlastRadiusBundle: %v", err)
	}
	if got := impactCounts(bundle.Diff.Impact); !reflect.DeepEqual(got, want) {
		t.Fatalf("blast-radius impact counts = %v, want %v", got, want)
	}
	if bundle.Diff.ImpactTotal != len(want) {
		t.Fatalf("impact_total = %d, want %d", bundle.Diff.ImpactTotal, len(want))
	}
	if bundle.Diff.ImpactKnown == nil || !*bundle.Diff.ImpactKnown {
		t.Fatalf("impact_known = %v, want true", bundle.Diff.ImpactKnown)
	}
	if bundle.Summary.FilesWithDependents != len(want) {
		t.Fatalf("files_with_dependents = %d, want %d", bundle.Summary.FilesWithDependents, len(want))
	}
	// The rendered diff panel uses canonical paths, never a bare basename.
	for _, path := range []string{"src/auth/util.ts", "src/payments/util.ts"} {
		if !strings.Contains(bundle.Rendered.Diff, path+" is used by") {
			t.Fatalf("rendered diff missing canonical impact line for %s:\n%s", path, bundle.Rendered.Diff)
		}
	}
}

// Capping the displayed changed files must not change the full statistics:
// ImpactTotal and the summary keep all dependents, and the footer discloses
// the omitted impact entries.
func TestImpactTruncationKeepsFullStatistics(t *testing.T) {
	requireBlastRadiusTools(t)
	root := makeSameNameImpactRepo(t)

	out, stderr, err := runCodemapWithInput("", "blast-radius", "--json", "--ref", "main",
		"--max-changed-files", "2", root)
	if err != nil {
		t.Fatalf("blast-radius failed: %v\nstderr=%s", err, stderr)
	}
	var bundle blastRadiusBundle
	if err := json.Unmarshal([]byte(out), &bundle); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if bundle.Diff.ImpactTotal != 4 {
		t.Fatalf("impact_total = %d, want full 4 despite the cap", bundle.Diff.ImpactTotal)
	}
	if len(bundle.Diff.Impact) > 2 {
		t.Fatalf("shown impact entries = %d, want <= shown files (2)", len(bundle.Diff.Impact))
	}
	// Every retained line keeps its full unique-importer count.
	for _, entry := range bundle.Diff.Impact {
		var full int
		switch entry.File {
		case "src/auth/util.ts", "src/lib/thing.ts", "src/react.ts":
			full = 1
		case "src/payments/util.ts":
			full = 2
		default:
			t.Fatalf("unexpected capped entry %+v", entry)
		}
		if entry.UsedBy != full {
			t.Fatalf("%s UsedBy = %d after cap, want full %d", entry.File, entry.UsedBy, full)
		}
	}
	if bundle.Summary.FilesWithDependents != 4 {
		t.Fatalf("files_with_dependents = %d, want full 4", bundle.Summary.FilesWithDependents)
	}
	if !strings.Contains(bundle.Rendered.Diff, "omitted from display") {
		t.Fatalf("rendered diff must disclose impact omission:\n%s", bundle.Rendered.Diff)
	}
}

// The CLI --diff JSON view reports the same canonical graph impact.
func TestImpactCLIDiffMatchesGraph(t *testing.T) {
	requireBlastRadiusTools(t)
	root := makeSameNameImpactRepo(t)

	out, stderr, err := runCodemapWithInput("", "--diff", "--json", root)
	if err != nil {
		t.Fatalf("--diff failed: %v\nstderr=%s", err, stderr)
	}
	var project scanner.Project
	if err := json.Unmarshal([]byte(out), &project); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	want := map[string]int{
		"src/auth/util.ts":     1,
		"src/payments/util.ts": 2,
		"src/lib/thing.ts":     1,
		"src/react.ts":         1,
	}
	if got := impactCounts(project.Impact); !reflect.DeepEqual(got, want) {
		t.Fatalf("CLI impact counts = %v, want %v", got, want)
	}
	if project.ImpactTotal != len(want) || project.ImpactKnown == nil || !*project.ImpactKnown {
		t.Fatalf("impact metadata = total %d known %v, want total %d and known", project.ImpactTotal, project.ImpactKnown, len(want))
	}
	paths := make([]string, 0, len(project.Impact))
	for _, entry := range project.Impact {
		paths = append(paths, entry.File)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if filepath.Base(path) == path {
			t.Fatalf("impact entry %q is a bare basename, want canonical path", path)
		}
	}
}
