package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/daemon"
	"github.com/yoanbernabeu/grepai/git"
)

var installInitService = installProjectService

func init() {
	command := &cobra.Command{
		Use: "service", Short: "Manage macOS login watchers",
		Long: "Manage user LaunchAgents. Watchers start at login and restart on exit.\nUse service uninstall to stop permanently; watch --stop only stops the current process.\nShell environment variables are not copied into login services.",
	}
	var workspace, legacyLogDir string
	command.PersistentFlags().StringVar(&legacyLogDir, "legacy-log-dir", "", "Previous custom workspace log directory to check before installation")
	command.PersistentFlags().StringVar(&workspace, "workspace", "", "Workspace name (otherwise inferred from the current project)")
	for _, action := range []string{"install", "uninstall", "status"} {
		command.AddCommand(&cobra.Command{
			Use: action, Short: action + " the login watcher", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				s, err := resolveLoginService(cwd, workspace)
				if err != nil {
					return err
				}
				switch action {
				case "install":
					if legacyLogDir != "" {
						if s.Workspace == "" {
							return fmt.Errorf("--legacy-log-dir requires a workspace target")
						}
						if err := checkWorkspaceServiceConflict(s.Workspace, legacyLogDir); err != nil {
							return err
						}
					}
					return installService(cmd.Context(), s)
				case "uninstall":
					if err := s.Uninstall(cmd.Context()); err != nil {
						return err
					}
					fmt.Printf("Removed login watcher: %s\n", s.Label)
				default:
					out, err := s.Status(cmd.Context())
					fmt.Printf("Registration: %s\nLogs: %s\n%s", s.Path, s.LogDir, out)
					return err
				}
				return nil
			},
		})
	}
	rootCmd.AddCommand(command)
}

func resolveLoginService(directory, workspace string) (*daemon.LoginService, error) {
	if workspace == "" {
		if info, err := git.Detect(directory); err == nil && info.IsWorktree {
			return nil, fmt.Errorf("login watchers are not installed for linked worktrees; use the main checkout")
		}
		match, err := config.MatchWorkspaceForPath(directory)
		if err != nil {
			return nil, err
		}
		if match != nil {
			workspace = match.Name
		} else {
			// Resolve from a subdirectory without changing the caller's working directory.
			for root := canonicalPath(directory); ; root = filepath.Dir(root) {
				if config.Exists(root) {
					directory = root
					break
				}
				if filepath.Dir(root) == root {
					break
				}
			}
		}
	}
	return daemon.NewLoginService(directory, workspace)
}

func installProjectService(ctx context.Context, directory string) error {
	s, err := resolveLoginService(directory, "")
	if err != nil {
		return err
	}
	return installService(ctx, s)
}

func installService(ctx context.Context, s *daemon.LoginService) error {
	loaded, err := s.Loaded(ctx)
	if err != nil {
		return err
	}
	if !loaded {
		projects := []string{s.Directory}
		if s.Workspace != "" {
			cfg, err := config.LoadWorkspaceConfig()
			if err != nil {
				return err
			}
			ws, err := cfg.GetWorkspace(s.Workspace)
			if err != nil {
				return err
			}
			hint, err := readWorkspaceLogDirHint(s.Workspace)
			if err != nil {
				return err
			}
			if err := checkWorkspaceServiceConflict(s.Workspace, s.LogDir, hint); err != nil {
				return err
			}
			projects = nil
			for _, p := range ws.Projects {
				projects = append(projects, p.Path)
			}
		} else if !config.Exists(s.Directory) {
			return fmt.Errorf("run grepai init before installing a login watcher")
		}
		for _, project := range projects {
			if err := checkServiceConflict(ctx, project, s.Label); err != nil {
				return err
			}
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := s.Install(ctx, executable); err != nil {
		return err
	}
	if s.Workspace != "" {
		if err := saveWorkspaceLogDirHint(s.Workspace, s.LogDir); err != nil {
			return fmt.Errorf("service registered, but could not save workspace log directory: %w", err)
		}
	} else {
		if err := saveWatchLogDirHint(s.Directory, s.LogDir); err != nil {
			return fmt.Errorf("service registered, but could not save log directory: %w", err)
		}
	}
	fmt.Printf("Login watcher registered: %s\nLogs: %s\n", s.Label, s.LogDir)
	fmt.Println("Starts at login; retries on exit. Registration does not mean indexing is ready.")
	fmt.Println("Use 'grepai service status' to inspect it and 'grepai service uninstall' to stop permanently.")
	fmt.Println("Login services do not inherit shell API keys; configure credentials separately before using a cloud embedder.")
	return nil
}

func checkWorkspaceServiceConflict(workspace string, dirs ...string) error {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		pid, err := daemon.GetRunningWorkspacePID(dir, workspace)
		if err != nil {
			return err
		}
		if pid != 0 {
			return fmt.Errorf("workspace watcher already running (PID %d, logs %s); stop it before installing the service", pid, dir)
		}
	}
	return nil
}

func checkServiceConflict(ctx context.Context, project, label string) error {
	s, err := daemon.NewLoginService(project, "")
	if err != nil {
		return err
	}
	if s.Label != label {
		loaded, err := s.Loaded(ctx)
		if err != nil {
			return err
		}
		if _, err := os.Stat(s.Path); err == nil || loaded {
			return fmt.Errorf("project %s has a standalone login watcher; uninstall it before installing the workspace service", project)
		}
	}
	dirs, err := resolveWatcherCandidateLogDirs(project)
	if err != nil {
		return err
	}
	dirs = append(dirs, s.LogDir)
	for _, dir := range dirs {
		pid, err := daemon.GetRunningPID(dir)
		if err != nil {
			return err
		}
		if pid == 0 {
			if info, err := git.Detect(project); err == nil && info.WorktreeID != "" {
				pid, err = daemon.GetRunningWorktreePID(dir, info.WorktreeID)
				if err != nil {
					return err
				}
			}
		}
		if pid != 0 {
			return fmt.Errorf("watcher already running (PID %d, logs %s); stop the existing watcher before installing a login service", pid, dir)
		}
	}
	return nil
}

// A registered service owns startup even while launchd is waiting to retry.
func checkManualServiceStart(directory, workspace string) error {
	if runtime.GOOS != "darwin" || watchStatus || watchStop || os.Getenv("GREPAI_BACKGROUND") == "1" {
		return nil
	}
	s, err := daemon.NewLoginService(directory, workspace)
	if err != nil {
		return err
	}
	if _, err := os.Stat(s.Path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("login service %s owns this watcher; use 'grepai service status' or 'grepai service uninstall' for this target before starting manually", s.Label)
}

func initLoginService(ctx context.Context, directory string) error {
	if initNoAutostart || runtime.GOOS != "darwin" {
		return nil
	}
	if info, err := git.Detect(directory); err == nil && info.IsWorktree {
		fmt.Println("Skipping login watcher for linked worktree.")
		return nil
	}
	if err := installInitService(ctx, directory); err != nil {
		return fmt.Errorf("configuration saved, but login watcher installation failed: %w; retry with 'grepai service install' or use 'grepai init --no-autostart'", err)
	}
	return nil
}
