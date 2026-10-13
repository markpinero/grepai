package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/daemon"
	"github.com/yoanbernabeu/grepai/trace"
)

var (
	migrateWorkspace      string
	migrateKeepLegacy     bool
	migrateDefaultIgnores bool
	migrateDryRun         bool
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate a project indexed by an earlier grepai release",
	Long: `Migrate a project's .grepai directory to the current on-disk layout.

The symbol index used by trace and refs moved from symbols.gob to
symbols.db (SQLite). A newer binary imports symbols.gob automatically the
first time it opens a project, but that happens silently inside the first
trace or watch call and can take tens of seconds on a large repository.
This command runs the same import with progress output, verifies the
imported row counts, and removes the old file.

Stop any watcher for the project first: a watcher started from an older
release keeps rewriting symbols.gob.

Optionally, --apply-default-ignores appends ignore patterns that newer
releases add by default (lockfiles, test scaffolding) to a config.yaml
written by an older release.`,
	RunE: runMigrate,
}

func init() {
	rootCmd.AddCommand(migrateCmd)
	migrateCmd.Flags().StringVar(&migrateWorkspace, "workspace", "", "Migrate every project in the named workspace instead of the current project")
	migrateCmd.Flags().BoolVar(&migrateKeepLegacy, "keep-legacy", false, "Keep symbols.gob as symbols.gob.migrated instead of deleting it")
	migrateCmd.Flags().BoolVar(&migrateDefaultIgnores, "apply-default-ignores", false, "Append missing default ignore patterns to the project's config.yaml")
	migrateCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false, "Report what would change without changing anything")
}

type migrateTarget struct {
	name string
	root string
}

func runMigrate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	targets, err := resolveMigrateTargets()
	if err != nil {
		return err
	}

	if migrateWorkspace != "" {
		if logDir, err := daemon.GetDefaultLogDir(); err == nil {
			if pid, _ := daemon.GetRunningWorkspacePID(logDir, migrateWorkspace); pid > 0 {
				return fmt.Errorf("workspace watcher for %q is running (PID %d); stop it first with: grepai watch --workspace %s --stop", migrateWorkspace, pid, migrateWorkspace)
			}
		}
	}

	var failed int
	for i, t := range targets {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s (%s)\n", t.name, t.root)
		if err := migrateProject(ctx, t.root); err != nil {
			failed++
			fmt.Printf("  error: %v\n", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d projects failed to migrate", failed, len(targets))
	}
	return nil
}

func resolveMigrateTargets() ([]migrateTarget, error) {
	if migrateWorkspace == "" {
		root, err := config.FindProjectRoot()
		if err != nil {
			return nil, err
		}
		return []migrateTarget{{name: filepath.Base(root), root: root}}, nil
	}

	wsCfg, err := config.LoadWorkspaceConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load workspace config: %w", err)
	}
	if wsCfg == nil {
		return nil, fmt.Errorf("no workspaces configured")
	}
	ws, err := wsCfg.GetWorkspace(migrateWorkspace)
	if err != nil {
		return nil, err
	}
	targets := make([]migrateTarget, 0, len(ws.Projects))
	for _, p := range ws.Projects {
		targets = append(targets, migrateTarget{name: p.Name, root: p.Path})
	}
	return targets, nil
}

func migrateProject(ctx context.Context, root string) error {
	if migrateWorkspace == "" {
		if st := resolveWatcherRuntimeStatus(root); st.running {
			// The single-project PID file is shared across projects, so
			// confirm from the log which project the watcher serves.
			watched := lastWatchedProject(st.logFile)
			switch {
			case watched == "":
				fmt.Printf("  warning: a watcher is running (PID %d); if it watches this project from an older release, stop it first\n", st.pid)
			case canonicalPath(watched) == canonicalPath(root):
				return fmt.Errorf("watcher is running (PID %d); stop it first with: grepai watch --stop", st.pid)
			}
		}
	}

	if err := migrateSymbolIndex(ctx, root); err != nil {
		return err
	}
	if migrateDefaultIgnores {
		if err := migrateIgnoreDefaults(root); err != nil {
			return err
		}
	}
	return nil
}

func migrateSymbolIndex(ctx context.Context, root string) error {
	gobPath := config.GetSymbolIndexPath(root)
	dbPath := config.GetSymbolDBPath(root)
	stale := []string{gobPath + ".migrated", gobPath + ".lock"}

	gobInfo, gobErr := os.Stat(gobPath)
	hasGob := gobErr == nil

	if hasGob {
		if _, err := os.Stat(dbPath); err == nil {
			st := trace.NewSQLiteSymbolStore(dbPath)
			stats, statsErr := st.GetStats(ctx)
			_ = st.Close()
			if statsErr == nil && stats.TotalFiles > 0 {
				fmt.Printf("  symbols.db already holds %d files; leaving %s untouched (delete symbols.db to re-import)\n", stats.TotalFiles, filepath.Base(gobPath))
				hasGob = false
				stale = stale[:1]
			}
		}
	}

	if hasGob {
		fmt.Printf("  symbol index: %s (%s) -> %s\n", filepath.Base(gobPath), formatBytes(gobInfo.Size()), filepath.Base(dbPath))
		if migrateDryRun {
			fmt.Println("  dry run: would import and verify, then remove the old file")
		} else {
			var progress trace.LegacyImportProgress
			var lastStage string
			if isInteractiveTerminal() {
				progress = func(stage string, done, total int) {
					if stage != lastStage {
						if lastStage != "" {
							fmt.Println()
						}
						lastStage = stage
					}
					fmt.Printf("\r  importing %-10s %8d / %d", stage, done, total)
				}
			}
			res, err := trace.ImportLegacySymbolIndex(ctx, gobPath, dbPath, progress)
			if lastStage != "" {
				fmt.Println()
			}
			if err != nil {
				return fmt.Errorf("symbol index import failed: %w", err)
			}
			fmt.Printf("  imported %d symbols, %d references, %d call edges across %d files in %s; counts verified\n",
				res.Symbols, res.References, res.CallEdges, res.Files, res.Duration.Round(time.Millisecond))
			if migrateKeepLegacy {
				fmt.Printf("  kept old index at %s\n", filepath.Base(res.LegacyPath))
				stale = stale[1:]
			}
		}
	} else if gobErr != nil {
		fmt.Println("  symbol index: nothing to import")
	}

	var reclaimed int64
	for _, p := range stale {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		size := ""
		if info.Size() > 0 {
			size = " (" + formatBytes(info.Size()) + ")"
		}
		if migrateDryRun {
			fmt.Printf("  dry run: would remove %s%s\n", filepath.Base(p), size)
			continue
		}
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("failed to remove %s: %w", p, err)
		}
		reclaimed += info.Size()
		fmt.Printf("  removed %s%s\n", filepath.Base(p), size)
	}
	if reclaimed > 0 {
		fmt.Printf("  reclaimed %s\n", formatBytes(reclaimed))
	}
	return nil
}

// lastWatchedProject returns the project path from the most recent
// "Starting grepai watch in <path>" line of a watcher log, or "" if unknown.
func lastWatchedProject(logFile string) string {
	data, err := os.ReadFile(logFile)
	if err != nil {
		return ""
	}
	const marker = "Starting grepai watch in "
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if idx := strings.Index(lines[i], marker); idx >= 0 {
			return strings.TrimSpace(lines[i][idx+len(marker):])
		}
	}
	return ""
}

func migrateIgnoreDefaults(root string) error {
	if !config.Exists(root) {
		fmt.Println("  ignore defaults: no config.yaml")
		return nil
	}
	cfg, err := config.Load(root)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	var missing []string
	for _, pattern := range config.DefaultConfig().Ignore {
		if !slices.Contains(cfg.Ignore, pattern) {
			missing = append(missing, pattern)
		}
	}
	if len(missing) == 0 {
		fmt.Println("  ignore defaults: already present")
		return nil
	}
	if migrateDryRun {
		fmt.Printf("  dry run: would add ignore patterns: %s\n", strings.Join(missing, ", "))
		return nil
	}
	cfg.Ignore = append(cfg.Ignore, missing...)
	if err := cfg.Save(root); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}
	fmt.Printf("  added ignore patterns: %s\n", strings.Join(missing, ", "))
	fmt.Println("  the next watch run removes newly ignored files from the index")
	return nil
}
