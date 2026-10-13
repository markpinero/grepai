package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/alpkeskin/gotoon"
	"github.com/spf13/cobra"
	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/embedder"
	"github.com/yoanbernabeu/grepai/rpg"
	gstats "github.com/yoanbernabeu/grepai/stats"
	"github.com/yoanbernabeu/grepai/trace"
)

var (
	traceMode      string
	traceDepth     int
	traceJSON      bool
	traceTOON      bool
	traceUI        bool
	traceWorkspace string
	traceProject   string
)

var runTraceActionCardUIRunner = runTraceActionCardUI

var traceCmd = &cobra.Command{
	Use:   "trace <subcommand> <symbol>",
	Short: "Trace symbol callers and callees",
	Long: `Trace command helps you understand code dependencies by finding:
- callers: functions that call the specified symbol
- callees: functions that the specified symbol calls
- graph: full call graph visualization

Examples:
  grepai trace callers "Login"
  grepai trace callees "HandleRequest" --mode precise
  grepai trace graph "ProcessOrder" --depth 3 --json`,
}

var traceCallersCmd = &cobra.Command{
	Use:   "callers <symbol>",
	Short: "Find all functions that call the specified symbol",
	Long: `Find all functions that call the specified symbol.

Examples:
  grepai trace callers "Login"
  grepai trace callers "HandleRequest" --json
  grepai trace callers "ProcessOrder" --mode precise`,
	Args: cobra.ExactArgs(1),
	RunE: runTraceCallers,
}

var traceCalleesCmd = &cobra.Command{
	Use:   "callees <symbol>",
	Short: "Find all functions called by the specified symbol",
	Long: `Find all functions called by the specified symbol.

Examples:
  grepai trace callees "Login"
  grepai trace callees "HandleRequest" --json`,
	Args: cobra.ExactArgs(1),
	RunE: runTraceCallees,
}

var traceGraphCmd = &cobra.Command{
	Use:   "graph <symbol>",
	Short: "Build a call graph around the specified symbol",
	Long: `Build a call graph showing callers and callees around a symbol.

Examples:
  grepai trace graph "Login" --depth 2
  grepai trace graph "HandleRequest" --depth 3 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runTraceGraph,
}

func init() {
	// Add flags to all trace subcommands
	for _, cmd := range []*cobra.Command{traceCallersCmd, traceCalleesCmd, traceGraphCmd} {
		cmd.Flags().StringVarP(&traceMode, "mode", "m", "fast", "Extraction mode: fast (regex) or precise (tree-sitter)")
		cmd.Flags().BoolVar(&traceJSON, "json", false, "Output results in JSON format")
		cmd.Flags().BoolVarP(&traceTOON, "toon", "t", false, "Output results in TOON format (token-efficient for AI agents)")
		cmd.Flags().BoolVar(&traceUI, "ui", false, "Show interactive UI output")
		cmd.MarkFlagsMutuallyExclusive("json", "toon")
		cmd.MarkFlagsMutuallyExclusive("json", "ui")
		cmd.MarkFlagsMutuallyExclusive("toon", "ui")
		cmd.Flags().StringVar(&traceWorkspace, "workspace", "", "Workspace name (inferred from cwd when omitted)")
		cmd.Flags().StringVar(&traceProject, "project", "", "Project name within workspace (inferred from cwd when omitted)")
	}
	traceGraphCmd.Flags().IntVarP(&traceDepth, "depth", "d", 2, "Maximum depth for graph traversal")

	traceCmd.AddCommand(traceCallersCmd)
	traceCmd.AddCommand(traceCalleesCmd)
	traceCmd.AddCommand(traceGraphCmd)

	rootCmd.AddCommand(traceCmd)
}

// traceStores opens the symbol stores a trace or refs command runs against:
// every project in the workspace when one is given or inferred, otherwise the
// current project. projectRoot is empty in workspace mode.
func traceStores(ctx context.Context, workspace, project string) (stores []trace.SymbolStore, projectRoot string, err error) {
	if workspace != "" {
		stores, err = trace.LoadWorkspaceSymbolStores(ctx, workspace, project)
		return stores, "", err
	}
	projectRoot, err = config.FindProjectRoot()
	if err != nil {
		return nil, "", err
	}
	st, err := trace.OpenProjectStore(ctx, config.GetSymbolDBPath(projectRoot))
	if err != nil {
		return nil, "", err
	}
	return []trace.SymbolStore{st}, projectRoot, nil
}

// traceIndexError renders an index error as an action card when --ui is set.
func traceIndexError(err error) error {
	if !traceUI {
		return err
	}
	if errors.Is(err, trace.ErrEmptyIndex) {
		return showTraceActionCardUIError(err, "Symbol index is empty", "Trace requires symbols extracted by the watcher.", "Run `grepai watch`")
	}
	return showTraceActionCardUIError(err, "Trace unavailable", err.Error(), "Run `grepai watch` first to build the index")
}

// prepareTrace validates the workspace flags and opens the stores.
func prepareTrace(ctx context.Context) (*trace.Query, []trace.SymbolStore, string, error) {
	if err := applyInferredWorkspace(&traceWorkspace, &traceProject); err != nil {
		return nil, nil, "", err
	}
	if traceProject != "" && traceWorkspace == "" {
		return nil, nil, "", fmt.Errorf("--project requires --workspace")
	}
	stores, projectRoot, err := traceStores(ctx, traceWorkspace, traceProject)
	if err != nil {
		return nil, nil, "", traceIndexError(err)
	}
	q := trace.NewQuery(stores...)
	q.Mode = traceMode
	return q, stores, projectRoot, nil
}

// finishTrace enriches and prints a result. Workspace results (empty
// projectRoot) have no single RPG graph or stats file, so they are printed
// as-is; an unknown symbol is printed without recording stats.
func finishTrace(result trace.TraceResult, view traceViewKind, projectRoot, commandType string, resultCount int) error {
	if projectRoot == "" || (result.Symbol == nil && view != traceViewGraph) {
		return outputTraceResult(result, view)
	}
	if cfg, err := config.Load(projectRoot); err != nil {
		log.Printf("Warning: failed to load config for RPG enrichment: %v", err)
	} else {
		enrichTraceWithRPG(projectRoot, cfg, &result)
	}
	return outputAndRecord(result, view, projectRoot, commandType, resultCount)
}

func runTraceCallers(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	q, stores, projectRoot, err := prepareTrace(ctx)
	if err != nil {
		return err
	}
	defer trace.CloseSymbolStores(stores)

	result := q.Callers(ctx, args[0])
	return finishTrace(result, traceViewCallers, projectRoot, gstats.TraceCallers, len(result.Callers))
}

func runTraceCallees(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	q, stores, projectRoot, err := prepareTrace(ctx)
	if err != nil {
		return err
	}
	defer trace.CloseSymbolStores(stores)

	result := q.Callees(ctx, args[0])
	return finishTrace(result, traceViewCallees, projectRoot, gstats.TraceCallees, len(result.Callees))
}

func runTraceGraph(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	q, stores, projectRoot, err := prepareTrace(ctx)
	if err != nil {
		return err
	}
	defer trace.CloseSymbolStores(stores)

	result, err := q.Graph(ctx, args[0], traceDepth)
	if err != nil {
		return err
	}
	return finishTrace(result, traceViewGraph, projectRoot, gstats.TraceGraph, len(result.Graph.Nodes))
}

func outputAndRecord(result trace.TraceResult, view traceViewKind, projectRoot, commandType string, resultCount int) error {
	if traceJSON {
		outputStr := captureJSON(result)
		fmt.Print(outputStr)
		recordTraceStats(projectRoot, commandType, resultCount, outputStr)
		return nil
	}
	if traceTOON {
		outputStr, err := captureTOON(result)
		if err != nil {
			return err
		}
		fmt.Print(outputStr)
		recordTraceStats(projectRoot, commandType, resultCount, outputStr)
		return nil
	}
	if err := outputTraceResult(result, view); err != nil {
		return err
	}
	recordTraceStats(projectRoot, commandType, resultCount, "")
	return nil
}

// enrichTraceWithRPG enriches all symbols in a TraceResult with RPG feature paths.
func enrichTraceWithRPG(projectRoot string, cfg *config.Config, result *trace.TraceResult) {
	if !cfg.RPG.Enabled {
		return
	}

	ctx := context.Background()
	rpgStore := rpg.NewGOBRPGStore(config.GetRPGIndexPath(projectRoot))
	if err := rpgStore.Load(ctx); err != nil {
		return // best-effort
	}
	defer rpgStore.Close()

	graph := rpgStore.GetGraph()
	qe := rpg.NewQueryEngine(graph)

	lookupFeaturePath := func(sym *trace.Symbol) {
		if sym == nil || sym.File == "" {
			return
		}
		nodes := graph.GetNodesByFile(sym.File)
		for _, n := range nodes {
			if n.Kind == rpg.KindSymbol && n.SymbolName == sym.Name {
				fetchResult, err := qe.FetchNode(ctx, rpg.FetchNodeRequest{NodeID: n.ID})
				if err == nil && fetchResult != nil {
					sym.FeaturePath = fetchResult.FeaturePath
				}
				return
			}
		}
	}

	// Enrich the main symbol
	if result.Symbol != nil {
		lookupFeaturePath(result.Symbol)
	}

	// Enrich callers
	for i := range result.Callers {
		lookupFeaturePath(&result.Callers[i].Symbol)
	}

	// Enrich callees
	for i := range result.Callees {
		lookupFeaturePath(&result.Callees[i].Symbol)
	}

	// Enrich graph nodes
	if result.Graph != nil {
		for name, sym := range result.Graph.Nodes {
			lookupFeaturePath(&sym)
			result.Graph.Nodes[name] = sym
		}
	}
}

type traceViewKind int

const (
	traceViewCallers traceViewKind = iota
	traceViewCallees
	traceViewGraph
)

func outputTraceResult(result trace.TraceResult, view traceViewKind) error {
	if traceJSON {
		return outputJSON(result)
	}
	if traceTOON {
		return outputTOON(result)
	}
	if traceUI {
		return runTraceResultUI(result, view)
	}

	if result.Symbol == nil && view != traceViewGraph {
		fmt.Printf("No symbol found: %s\n", result.Query)
		return nil
	}

	switch view {
	case traceViewCallers:
		return displayCallersResult(result)
	case traceViewCallees:
		return displayCalleesResult(result)
	case traceViewGraph:
		return displayGraphResult(result)
	default:
		return nil
	}
}

func showTraceActionCardUIError(displayErr error, title, why, action string) error {
	if uiErr := runTraceActionCardUIRunner(title, why, action); uiErr != nil {
		return fmt.Errorf("%w (failed to render trace UI card: %v)", displayErr, uiErr)
	}
	return displayErr
}

// captureJSON serializes a TraceResult to a JSON string without writing to stdout.
func captureJSON(result trace.TraceResult) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result)
	return buf.String()
}

func outputJSON(result trace.TraceResult) error {
	fmt.Print(captureJSON(result))
	return nil
}

func outputTOON(result trace.TraceResult) error {
	s, err := captureTOON(result)
	if err != nil {
		return err
	}
	fmt.Print(s)
	return nil
}

// captureTOON serializes a TraceResult to a TOON string without writing to stdout.
func captureTOON(result trace.TraceResult) (string, error) {
	output, err := gotoon.Encode(result)
	if err != nil {
		return "", fmt.Errorf("failed to encode TOON: %w", err)
	}
	return output + "\n", nil
}

// recordTraceStats fires a goroutine to record a trace stats entry without blocking.
func recordTraceStats(projectRoot, commandType string, resultCount int, outputStr string) {
	rec := gstats.NewRecorder(projectRoot)
	entry := gstats.Entry{
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		CommandType:  commandType,
		OutputMode:   outputModeFromFlags(traceJSON, traceTOON, false),
		ResultCount:  resultCount,
		OutputTokens: embedder.EstimateTokens(outputStr),
		GrepTokens:   gstats.GrepEquivalentTokens(resultCount),
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_ = rec.Record(ctx, entry)
	}()
}

func displayCallersResult(result trace.TraceResult) error {
	fmt.Printf("Symbol: %s (%s)\n", result.Symbol.Name, result.Symbol.Kind)
	fmt.Printf("File: %s:%d\n", result.Symbol.File, result.Symbol.Line)
	if result.Symbol.FeaturePath != "" {
		fmt.Printf("Feature: %s\n", result.Symbol.FeaturePath)
	}
	fmt.Printf("\nCallers (%d):\n", len(result.Callers))
	fmt.Println(strings.Repeat("-", 60))

	if len(result.Callers) == 0 {
		fmt.Println("No callers found.")
		return nil
	}

	for i, caller := range result.Callers {
		fmt.Printf("\n%d. %s\n", i+1, caller.Symbol.Name)
		if caller.Symbol.File != "" {
			fmt.Printf("   Defined: %s:%d\n", caller.Symbol.File, caller.Symbol.Line)
		}
		if caller.Symbol.FeaturePath != "" {
			fmt.Printf("   Feature: %s\n", caller.Symbol.FeaturePath)
		}
		fmt.Printf("   Calls at: %s:%d\n", caller.CallSite.File, caller.CallSite.Line)
		if caller.CallSite.Context != "" {
			fmt.Printf("   Context: %s\n", truncate(caller.CallSite.Context, 80))
		}
	}

	return nil
}

func displayCalleesResult(result trace.TraceResult) error {
	fmt.Printf("Symbol: %s (%s)\n", result.Symbol.Name, result.Symbol.Kind)
	fmt.Printf("File: %s:%d\n", result.Symbol.File, result.Symbol.Line)
	if result.Symbol.FeaturePath != "" {
		fmt.Printf("Feature: %s\n", result.Symbol.FeaturePath)
	}
	fmt.Printf("\nCallees (%d):\n", len(result.Callees))
	fmt.Println(strings.Repeat("-", 60))

	if len(result.Callees) == 0 {
		fmt.Println("No callees found.")
		return nil
	}

	for i, callee := range result.Callees {
		fmt.Printf("\n%d. %s\n", i+1, callee.Symbol.Name)
		if callee.Symbol.File != "" {
			fmt.Printf("   Defined: %s:%d\n", callee.Symbol.File, callee.Symbol.Line)
		}
		if callee.Symbol.FeaturePath != "" {
			fmt.Printf("   Feature: %s\n", callee.Symbol.FeaturePath)
		}
		fmt.Printf("   Called at: %s:%d\n", callee.CallSite.File, callee.CallSite.Line)
	}

	return nil
}

func displayGraphResult(result trace.TraceResult) error {
	fmt.Printf("Call Graph for: %s (depth: %d)\n", result.Query, result.Graph.Depth)
	fmt.Println(strings.Repeat("=", 60))

	fmt.Printf("\nNodes (%d):\n", len(result.Graph.Nodes))
	for name, sym := range result.Graph.Nodes {
		if sym.FeaturePath != "" {
			fmt.Printf("  - %s (%s) @ %s:%d [%s]\n", name, sym.Kind, sym.File, sym.Line, sym.FeaturePath)
		} else {
			fmt.Printf("  - %s (%s) @ %s:%d\n", name, sym.Kind, sym.File, sym.Line)
		}
	}

	fmt.Printf("\nEdges (%d):\n", len(result.Graph.Edges))
	for _, edge := range result.Graph.Edges {
		fmt.Printf("  %s -> %s [%s:%d]\n", edge.Caller, edge.Callee, edge.File, edge.Line)
	}

	return nil
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
