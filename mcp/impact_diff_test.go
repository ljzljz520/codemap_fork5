package codemapmcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codemap/scanner"
)

func writeImpactDiffRepo(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"package.json":             `{"name":"app","dependencies":{"react":"18.0.0"}}`,
		"tsconfig.json":            `{"compilerOptions":{"baseUrl":".","paths":{"@app/*":["src/*"]}}}`,
		"src/auth/util.ts":         "export const auth = 2;\n",
		"src/auth/client.ts":       "import { auth } from './util';\nconsole.log(auth);\n",
		"src/payments/util.ts":     "export const pay = 2;\n",
		"src/payments/checkout.ts": "import { pay } from './util';\nconsole.log(pay);\n",
		"src/payments/refund.ts":   "import { pay } from './util';\nconsole.log(pay);\n",
		"src/lib/thing.ts":         "export const thing = 2;\n",
		"src/consumer.ts":          "import { a } from '@app/lib/thing';\nimport { b } from './lib/thing';\nconsole.log(a, b);\n",
		"src/react.ts":             "export const local = 2;\n",
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
}

// MCP get_diff must answer from the same resolved graph as the CLI diff and
// blast-radius: canonical paths, unique importers, external packages excluded.
func TestMCPDiffImpactMatchesGraph(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if !scanner.NewAstGrepAnalyzer().Available() {
		t.Skip("ast-grep not available")
	}
	root := makeMCPGitRepo(t, "main")
	writeImpactDiffRepo(t, root)
	runGitMCPTestCmd(t, root, "add", ".")
	runGitMCPTestCmd(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "change")
	// Now modify the four targets again so they differ from main.
	for relPath, content := range map[string]string{
		"src/auth/util.ts":     "export const auth = 3;\n",
		"src/payments/util.ts": "export const pay = 3;\n",
		"src/lib/thing.ts":     "export const thing = 3;\n",
		"src/react.ts":         "export const local = 3;\n",
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(relPath)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Reference answer from the shared graph-based analyzer.
	diffInfo, err := scanner.GitDiffInfo(context.Background(), root, "main")
	if err != nil {
		t.Fatal(err)
	}
	all, err := scanner.ScanConfiguredFiles(context.Background(), root, scanner.NewGitIgnoreCache(root))
	if err != nil {
		t.Fatal(err)
	}
	changed := scanner.FilterToChangedWithInfo(all, diffInfo)
	reference, err := scanner.AnalyzeImpact(context.Background(), root, changed, scanner.ConfiguredFilters(root))
	if err != nil {
		t.Fatalf("AnalyzeImpact: %v", err)
	}

	result, payload, err := handleGetDiff(context.Background(), nil, DiffInput{Path: root, Ref: "main"})
	if err != nil {
		t.Fatalf("handleGetDiff: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_diff error: %s", resultText(t, result))
	}

	diff, ok := payload.(DiffOutput)
	if !ok {
		t.Fatalf("payload = %T, want DiffOutput", payload)
	}
	if diff.Project.ImpactKnown == nil || !*diff.Project.ImpactKnown {
		t.Fatalf("impact_known = %v, want true", diff.Project.ImpactKnown)
	}
	if diff.Project.ImpactTotal != len(reference.Entries) {
		t.Fatalf("impact_total = %d, want %d", diff.Project.ImpactTotal, len(reference.Entries))
	}
	got := make(map[string]int, len(diff.Project.Impact))
	for _, entry := range diff.Project.Impact {
		got[entry.File] = entry.UsedBy
	}
	want := map[string]int{
		"src/auth/util.ts":     1,
		"src/payments/util.ts": 2,
		"src/lib/thing.ts":     1,
		"src/react.ts":         1,
	}
	if len(got) != len(want) {
		t.Fatalf("MCP impact = %v, want %v", got, want)
	}
	for path, count := range want {
		if got[path] != count {
			t.Fatalf("MCP impact[%s] = %d, want %d (full = %v)", path, got[path], count, got)
		}
	}

	// The human-readable footer uses canonical paths for both same-named
	// files, never a bare basename.
	text := resultText(t, result)
	for _, path := range []string{"src/auth/util.ts", "src/payments/util.ts"} {
		if !strings.Contains(text, path+" is used by") {
			t.Fatalf("footer missing canonical line for %s:\n%s", path, text)
		}
	}
}

// Structured-output bounds truncate lists but never rewrite the counts:
// ImpactTotal survives, dropped entries show up in omitted_count, and a
// capped importer list leaves UsedBy at its full value.
func TestMCPDiffBoundsKeepFullCounts(t *testing.T) {
	importers := make([]string, maxStructuredItems+100)
	for i := range importers {
		importers[i] = fmt.Sprintf("importer_%04d", i)
	}
	// Hub sorts first (higher UsedBy) and must stay within the kept range;
	// the 501st entry is dropped by the bound.
	manyImpacters := []scanner.ImpactInfo{{
		File: "hub.ts", UsedBy: len(importers), Importers: importers,
	}}
	for i := 0; i < maxStructuredItems; i++ {
		manyImpacters = append(manyImpacters, scanner.ImpactInfo{File: "a.ts", UsedBy: 1})
	}
	known := true
	project := scanner.Project{
		Root:        t.TempDir(),
		Mode:        "tree",
		DiffRef:     "main",
		ImpactKnown: &known,
	}
	project.ApplyImpact(scanner.ImpactResult{Known: true, Entries: manyImpacters})
	if project.ImpactTotal != maxStructuredItems+1 {
		t.Fatalf("setup: impact_total = %d", project.ImpactTotal)
	}

	output := newDiffOutput("diff", "main", project)
	if !output.Truncated || output.OmittedCount != 1 {
		t.Fatalf("truncated=%v omitted=%d, want 1 omitted entry", output.Truncated, output.OmittedCount)
	}
	if output.Project.ImpactTotal != maxStructuredItems+1 {
		t.Fatalf("bounded impact_total = %d, want full %d", output.Project.ImpactTotal, maxStructuredItems+1)
	}
	if len(output.Project.Impact) != maxStructuredItems {
		t.Fatalf("bounded entries = %d, want %d", len(output.Project.Impact), maxStructuredItems)
	}
	hub := output.Project.Impact[0]
	if hub.File != "hub.ts" {
		t.Fatalf("first bounded entry = %s, want hub.ts", hub.File)
	}
	if hub.UsedBy != maxStructuredItems+100 {
		t.Fatalf("bounded hub UsedBy = %d, want full %d", hub.UsedBy, maxStructuredItems+100)
	}
	if len(hub.Importers) != maxStructuredItems {
		t.Fatalf("bounded hub importers = %d, want payload cap %d", len(hub.Importers), maxStructuredItems)
	}
	// The original project keeps its full hub entry: bounding must not
	// mutate its importer list or count.
	if got := project.Impact[0]; len(got.Importers) != maxStructuredItems+100 || got.UsedBy != maxStructuredItems+100 {
		t.Fatalf("original hub entry mutated: importers=%d usedBy=%d", len(got.Importers), got.UsedBy)
	}
}
