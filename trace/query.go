package trace

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
)

// ErrEmptyIndex is returned by OpenProjectStore when the symbol index exists
// but holds no symbols, which means the watcher has not run yet.
var ErrEmptyIndex = errors.New("symbol index is empty. Run 'grepai watch' first to build the index")

// OpenProjectStore opens a project's symbol index and verifies it holds
// symbols. The caller owns the returned store and must Close it.
func OpenProjectStore(ctx context.Context, dbPath string) (SymbolStore, error) {
	st := NewSQLiteSymbolStore(dbPath)
	if err := st.Load(ctx); err != nil {
		return nil, fmt.Errorf("failed to load symbol index: %w", err)
	}
	stats, err := st.GetStats(ctx)
	if err != nil || stats.TotalSymbols == 0 {
		_ = st.Close()
		return nil, ErrEmptyIndex
	}
	return st, nil
}

// Usage is one read or write of a symbol, with the symbol that performs it.
type Usage struct {
	Symbol   Symbol   `json:"symbol"`
	Access   string   `json:"access"`
	AccessAt CallSite `json:"access_at"`
}

// Query answers trace and refs questions over one or more symbol stores.
// Results from several stores (a workspace) are aggregated; a reference is
// resolved against the store it came from. Every surface that shows trace
// output (CLI, MCP) builds its result here so heuristics live in one place.
type Query struct {
	stores []SymbolStore
	// Mode is copied into every TraceResult ("fast" or "precise").
	Mode string
}

// NewQuery builds a Query over the given stores.
func NewQuery(stores ...SymbolStore) *Query {
	return &Query{stores: stores, Mode: "fast"}
}

// Callers returns the symbol and every call site that references it.
// Result.Symbol is nil when no store defines the symbol.
func (q *Query) Callers(ctx context.Context, name string) TraceResult {
	result := TraceResult{Query: name, Mode: q.Mode}
	for _, ss := range q.stores {
		refs, err := ss.LookupCallers(ctx, name)
		if err != nil {
			log.Printf("Warning: failed to lookup callers of %q: %v", name, err)
		}
		symbols, err := ss.LookupSymbol(ctx, name)
		if err != nil {
			log.Printf("Warning: failed to lookup symbol %q: %v", name, err)
		}
		if len(symbols) > 0 && result.Symbol == nil {
			result.Symbol = pickBestTargetSymbol(symbols, refs)
		}
		for _, ref := range refs {
			result.Callers = append(result.Callers, CallerInfo{
				Symbol:   resolveCaller(ctx, ss, ref),
				CallSite: CallSite{File: ref.File, Line: ref.Line, Context: ref.Context},
			})
		}
	}
	return result
}

// Callees returns the symbol and every call it makes.
func (q *Query) Callees(ctx context.Context, name string) TraceResult {
	result := TraceResult{Query: name, Mode: q.Mode}
	for _, ss := range q.stores {
		symbols, err := ss.LookupSymbol(ctx, name)
		if err != nil {
			log.Printf("Warning: failed to lookup symbol %q: %v", name, err)
		}
		if len(symbols) == 0 {
			continue
		}
		if result.Symbol == nil {
			result.Symbol = &symbols[0]
		}
		refs, err := ss.LookupCallees(ctx, name, symbols[0].File)
		if err != nil {
			log.Printf("Warning: failed to lookup callees of %q: %v", name, err)
		}
		for _, ref := range refs {
			calleeSym := Symbol{Name: ref.SymbolName}
			if calleeSyms, err := ss.LookupSymbol(ctx, ref.SymbolName); err != nil {
				log.Printf("Warning: failed to lookup callee symbol %q: %v", ref.SymbolName, err)
			} else if len(calleeSyms) > 0 {
				calleeSym = calleeSyms[0]
			}
			result.Callees = append(result.Callees, CalleeInfo{
				Symbol:   calleeSym,
				CallSite: CallSite{File: ref.File, Line: ref.Line, Context: ref.Context},
			})
		}
	}
	return result
}

// Graph builds the call graph around a symbol, merging graphs from every
// store and deduplicating edges by caller and callee.
func (q *Query) Graph(ctx context.Context, name string, depth int) (TraceResult, error) {
	result := TraceResult{Query: name, Mode: q.Mode}
	if len(q.stores) == 1 {
		graph, err := q.stores[0].GetCallGraph(ctx, name, depth)
		if err != nil {
			return result, fmt.Errorf("failed to build call graph: %w", err)
		}
		result.Graph = graph
		return result, nil
	}

	merged := &CallGraph{
		Root:  name,
		Nodes: make(map[string]Symbol),
		Edges: []CallEdge{},
		Depth: depth,
	}
	edgeSeen := make(map[string]bool)
	for _, ss := range q.stores {
		graph, err := ss.GetCallGraph(ctx, name, depth)
		if err != nil {
			log.Printf("Warning: failed to build call graph for %q: %v", name, err)
			continue
		}
		for nodeName, sym := range graph.Nodes {
			if _, exists := merged.Nodes[nodeName]; !exists {
				merged.Nodes[nodeName] = sym
			}
		}
		for _, edge := range graph.Edges {
			key := edge.Caller + "->" + edge.Callee
			if !edgeSeen[key] {
				merged.Edges = append(merged.Edges, edge)
				edgeSeen[key] = true
			}
		}
	}
	result.Graph = merged
	return result, nil
}

// References returns the reads (RefKindRead) or writes (RefKindWrite) of a
// symbol, each attributed to the symbol performing the access.
func (q *Query) References(ctx context.Context, name, kind string) []Usage {
	usages := make([]Usage, 0)
	for _, ss := range q.stores {
		var refs []Reference
		var err error
		if kind == RefKindWrite {
			refs, err = ss.LookupWriters(ctx, name)
		} else {
			refs, err = ss.LookupReaders(ctx, name)
		}
		if err != nil {
			log.Printf("Warning: failed to lookup refs for %q: %v", name, err)
			continue
		}
		for _, ref := range refs {
			usages = append(usages, Usage{
				Symbol:   resolveCaller(ctx, ss, ref),
				Access:   ref.Kind,
				AccessAt: CallSite{File: ref.File, Line: ref.Line, Context: ref.Context},
			})
		}
	}
	return usages
}

// resolveCaller finds the symbol a reference is made from, preferring a
// definition in the reference's own file. Top-level references and unknown
// callers fall back to a placeholder built from the reference.
func resolveCaller(ctx context.Context, ss SymbolStore, ref Reference) Symbol {
	placeholder := Symbol{Name: ref.CallerName, File: ref.CallerFile, Line: ref.CallerLine}
	if ref.CallerName == "" || ref.CallerName == "<top-level>" {
		return placeholder
	}
	candidates, err := ss.LookupSymbol(ctx, ref.CallerName)
	if err != nil {
		log.Printf("Warning: failed to lookup caller symbol %q: %v", ref.CallerName, err)
		return placeholder
	}
	if best := pickBestSymbolForFile(candidates, ref.CallerFile); best != nil {
		return *best
	}
	return placeholder
}

// pickBestTargetSymbol chooses which definition a trace is about when a name
// is defined more than once: the one most referenced from its own file, with
// a deterministic path/line fallback.
func pickBestTargetSymbol(candidates []Symbol, refs []Reference) *Symbol {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return &candidates[0]
	}

	bestIdx := 0
	bestScore := -1
	for i, sym := range candidates {
		score := 0
		for _, ref := range refs {
			if ref.File == sym.File || ref.CallerFile == sym.File {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}

	if bestScore <= 0 {
		sorted := append([]Symbol(nil), candidates...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].File == sorted[j].File {
				return sorted[i].Line < sorted[j].Line
			}
			return sorted[i].File < sorted[j].File
		})
		return &sorted[0]
	}

	return &candidates[bestIdx]
}

// pickBestSymbolForFile prefers the candidate defined in preferredFile.
func pickBestSymbolForFile(candidates []Symbol, preferredFile string) *Symbol {
	if len(candidates) == 0 {
		return nil
	}
	if preferredFile != "" {
		for i := range candidates {
			if candidates[i].File == preferredFile {
				return &candidates[i]
			}
		}
	}
	return &candidates[0]
}
