package trace

import "testing"

func TestPickBestTargetSymbol_prefersSameFileReferences(t *testing.T) {
	candidates := []Symbol{
		{Name: "isAdmin", File: "src/components/Navigation.vue", Line: 10},
		{Name: "isAdmin", File: "src/views/Home.vue", Line: 20},
	}
	refs := []Reference{
		{SymbolName: "isAdmin", File: "src/views/Home.vue", CallerFile: "src/views/Home.vue", Line: 49},
	}

	picked := pickBestTargetSymbol(candidates, refs)
	if picked == nil {
		t.Fatal("expected symbol to be selected")
	}
	if picked.File != "src/views/Home.vue" {
		t.Fatalf("expected Home.vue symbol, got %s", picked.File)
	}
}

func TestPickBestSymbolForFile_prefersCallerFile(t *testing.T) {
	candidates := []Symbol{
		{Name: "__vue_template_reads__", File: "src/App.vue", Line: 100},
		{Name: "__vue_template_reads__", File: "src/views/Home.vue", Line: 200},
	}

	picked := pickBestSymbolForFile(candidates, "src/views/Home.vue")
	if picked == nil {
		t.Fatal("expected caller symbol")
	}
	if picked.File != "src/views/Home.vue" {
		t.Fatalf("expected Home.vue caller symbol, got %s", picked.File)
	}
}
