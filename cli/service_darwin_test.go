package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/daemon"
)

func TestInitLoginRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	withInitTestState(t, project, func() {
		initNonInteractive = true
		initNoAutostart = false
	})
	old := installInitService
	t.Cleanup(func() { installInitService = old })
	calls := 0
	installInitService = func(_ context.Context, directory string) error {
		calls++
		if !config.Exists(directory) {
			t.Fatal("registration attempted before saving config")
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		if err := runInit(nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("new and existing init must ensure registration: got %d calls", calls)
	}
	initNoAutostart = true
	if err := runInit(nil, nil); err != nil || calls != 2 {
		t.Fatalf("opt-out registered a service: calls=%d, err=%v", calls, err)
	}
	initNoAutostart = false
	installInitService = func(context.Context, string) error { return fmt.Errorf("bootstrap failed") }
	if err := runInit(nil, nil); err == nil {
		t.Fatal("registration failure must be reported")
	}
	if _, err := config.Load(project); err != nil {
		t.Fatal("registration failure damaged config", err)
	}
}

func TestLoginServiceRouting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	if err := config.DefaultConfig().Save(project); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(project, "src")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := resolveLoginService(project, "")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := resolveLoginService(subdir, "")
	if err != nil || sub.Label != root.Label {
		t.Fatalf("subdirectory created distinct service: %v", err)
	}
	cfg := config.DefaultWorkspaceConfig()
	cfg.Workspaces["hum"] = config.Workspace{Name: "hum", Projects: []config.ProjectEntry{{Name: "api", Path: project}}}
	if err := config.SaveWorkspaceConfig(cfg); err != nil {
		t.Fatal(err)
	}
	inferred, err := resolveLoginService(subdir, "")
	if err != nil || inferred.Workspace != "hum" {
		t.Fatalf("failed to route workspace member: %v, %v", inferred, err)
	}
	explicit, err := resolveLoginService(t.TempDir(), "hum")
	if err != nil || explicit.Label != inferred.Label || explicit.Label == root.Label {
		t.Fatalf("workspace identity mismatch: %v", err)
	}
}

func TestLoginServiceRejectsLegacyWatcher(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	s, err := resolveLoginService(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.LogDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.LogDir, "grepai-watch.pid"), fmt.Appendf(nil, "%d\n", os.Getpid()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkServiceConflict(context.Background(), project, s.Label); err == nil {
		t.Fatal("did not reject existing watcher")
	}
}

func TestLoginServiceSkipsLinkedWorktrees(t *testing.T) {
	_, worktree := setupMainRepoForWorktreeDiscovery(t)
	t.Setenv("HOME", t.TempDir())
	if _, err := resolveLoginService(worktree, ""); err == nil {
		t.Fatal("linked worktree accepted as login service")
	}
	old := installInitService
	oldOptOut := initNoAutostart
	t.Cleanup(func() { installInitService = old; initNoAutostart = oldOptOut })
	initNoAutostart = false
	installInitService = func(context.Context, string) error {
		t.Fatal("worktree init must not register a service")
		return nil
	}
	if err := initLoginService(context.Background(), worktree); err != nil {
		t.Fatal(err)
	}
}

func withServiceWatchState(t *testing.T) {
	t.Helper()
	background, status, stop := watchBackground, watchStatus, watchStop
	workspace, logDir := watchWorkspace, watchLogDir
	runner := watchStopDaemonRunner
	t.Cleanup(func() {
		watchBackground, watchStatus, watchStop = background, status, stop
		watchWorkspace, watchLogDir = workspace, logDir
		watchStopDaemonRunner = runner
	})
	watchBackground, watchStatus, watchStop = false, false, false
	t.Setenv("GREPAI_BACKGROUND", "")
	watchWorkspace, watchLogDir = "", ""
	t.Setenv("HOME", t.TempDir())
}

func TestLoginServiceWatchRoutingAndCustomLogs(t *testing.T) {
	withServiceWatchState(t)
	project := t.TempDir()
	t.Chdir(project)
	cfg := config.DefaultWorkspaceConfig()
	cfg.Workspaces["hum"] = config.Workspace{Name: "hum", Store: config.StoreConfig{Backend: "qdrant"}, Projects: []config.ProjectEntry{{Name: "api", Path: project}}}
	if err := config.SaveWorkspaceConfig(cfg); err != nil {
		t.Fatal(err)
	}
	s, err := resolveLoginService(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte("registered service fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	custom := t.TempDir()
	if err := saveWorkspaceLogDirHint("hum", custom); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon.GetWorkspacePIDFile(custom, "hum"), fmt.Appendf(nil, "%d\n", os.Getpid()), 0600); err != nil {
		t.Fatal(err)
	}
	// Plain project watch must see the workspace PID, without starting a child.
	if err := runWatch(nil, nil); err == nil {
		t.Fatal("plain watch ignored registered workspace watcher")
	}
	if watchWorkspace != "hum" {
		t.Fatal("plain watch was not routed to workspace")
	}
	hint, err := readWorkspaceLogDirHint("hum")
	if err != nil || hint != custom {
		t.Fatalf("custom workspace logs lost: %q, %v", hint, err)
	}
	if err := checkWorkspaceServiceConflict("hum", s.LogDir, hint); err == nil {
		t.Fatal("custom-log workspace watcher did not block installation")
	}
	if err := os.Remove(daemon.GetWorkspacePIDFile(custom, "hum")); err != nil {
		t.Fatal(err)
	}
	if err := runWatch(nil, nil); err == nil {
		t.Fatal("manual workspace start allowed during service retry interval")
	}
}

func TestLoginServiceProjectRetryGuard(t *testing.T) {
	withServiceWatchState(t)
	project := t.TempDir()
	s, err := resolveLoginService(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte("registered"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, background := range []bool{false, true} {
		watchBackground = background
		if err := checkManualServiceStart(project, ""); err == nil {
			t.Fatal("manual start allowed for registered project without PID")
		}
	}
	watchStatus = true
	if err := checkManualServiceStart(project, ""); err != nil {
		t.Fatal("status was blocked", err)
	}
	watchStatus = false
	t.Setenv("GREPAI_BACKGROUND", "1")
	if err := checkManualServiceStart(project, ""); err != nil {
		t.Fatal("supervised child was blocked", err)
	}
}

func TestLoginServiceStopFallsBackFromStaleHint(t *testing.T) {
	withServiceWatchState(t)
	project := t.TempDir()
	t.Chdir(project)
	if err := config.DefaultConfig().Save(project); err != nil {
		t.Fatal(err)
	}
	custom := t.TempDir()
	if err := saveWatchLogDirHint(project, custom); err != nil {
		t.Fatal(err)
	}
	defaultDir, err := daemon.GetDefaultLogDir()
	if err != nil {
		t.Fatal(err)
	}
	var visited []string
	watchStopDaemonRunner = func(dir, _ string) (bool, error) {
		visited = append(visited, dir)
		return dir == defaultDir, nil
	}
	watchStop = true
	if err := runWatch(nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(visited) != 2 || visited[0] != custom || visited[1] != defaultDir {
		t.Fatalf("expected stale hint then default log directory: %v", visited)
	}
}
