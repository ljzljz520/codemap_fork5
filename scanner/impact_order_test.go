package scanner

import (
	"fmt"
	"reflect"
	"testing"

	"codemap/analysis"
)

// Impact comes from graph maps, and sorting by count alone left equal counts
// in map order: the diff impact footer and blast-radius JSON reordered between
// identical runs, and an output budget then kept a different subset each time.
func TestAnalyzeImpactOrderIsDeterministicAcrossTies(t *testing.T) {
	var changed []FileInfo
	fg := &FileGraph{
		Importers: make(map[string][]string),
		Coverage: GraphCoverage{Sources: []analysis.Source{
			{Name: "ast-grep", Status: ScanSourceAuthoritative, Languages: astGrepLanguages},
		}},
	}
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("src/c%02d.ts", i)
		changed = append(changed, FileInfo{Path: name})
		// every changed file is imported by exactly two others: all tied
		for j := 0; j < 2; j++ {
			fg.Importers[name] = append(fg.Importers[name], fmt.Sprintf("src/user%02d_%d.ts", i, j))
		}
	}
	// one file with more importers must lead
	fg.Importers["src/c07.ts"] = append(fg.Importers["src/c07.ts"], "src/extra.ts")

	first := AnalyzeImpactFromGraph(changed, fg)
	if len(first.Entries) != 12 {
		t.Fatalf("entries = %+v, want 12", first.Entries)
	}
	lead := first.Entries[0]
	if lead.File != "src/c07.ts" || lead.UsedBy != 3 {
		t.Fatalf("first = %+v, want src/c07.ts (3)", lead)
	}
	if lead.Granularity != ImpactGranularityFile {
		t.Fatalf("granularity = %q, want file", lead.Granularity)
	}
	for i := 1; i < len(first.Entries); i++ {
		if first.Entries[i-1].UsedBy == first.Entries[i].UsedBy && first.Entries[i-1].File > first.Entries[i].File {
			t.Fatalf("tie not broken by path: %s before %s", first.Entries[i-1].File, first.Entries[i].File)
		}
	}
	for run := 1; run < 8; run++ {
		if got := AnalyzeImpactFromGraph(changed, fg); !reflect.DeepEqual(got.Entries, first.Entries) {
			t.Fatalf("run %d = %+v, want %+v", run, got.Entries, first.Entries)
		}
	}
}

// A changed file with no importers is omitted rather than reported as zero.
func TestAnalyzeImpactOmitsZeroDependents(t *testing.T) {
	fg := &FileGraph{
		Importers: map[string][]string{"src/a.ts": {"src/user.ts"}},
		Coverage: GraphCoverage{Sources: []analysis.Source{
			{Name: "ast-grep", Status: ScanSourceAuthoritative, Languages: astGrepLanguages},
		}},
	}
	result := AnalyzeImpactFromGraph([]FileInfo{{Path: "src/a.ts"}, {Path: "src/b.ts"}}, fg)
	if len(result.Entries) != 1 || result.Entries[0].File != "src/a.ts" {
		t.Fatalf("entries = %+v, want only src/a.ts", result.Entries)
	}
	if !result.Known || result.CoverageStatus != analysis.CoverageComplete {
		t.Fatalf("coverage = known=%v status=%q, want known/complete", result.Known, result.CoverageStatus)
	}
}

// With no graph the answer is explicitly unknown, never a confident empty
// result that a reader would take for zero dependents.
func TestAnalyzeImpactFromNilGraphIsUnknown(t *testing.T) {
	result := AnalyzeImpactFromGraph([]FileInfo{{Path: "src/a.ts"}}, nil)
	if result.Known || result.CoverageStatus != analysis.CoverageUnavailable || len(result.Entries) != 0 {
		t.Fatalf("result = %+v, want unknown with no entries", result)
	}
	if len(result.CoverageNotes) == 0 {
		t.Fatalf("unknown result must explain itself, got %+v", result)
	}
}
