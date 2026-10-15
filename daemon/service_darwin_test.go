package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakeLaunchctl(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FAKE_STATE", filepath.Join(home, "loaded"))
	t.Setenv("FAKE_CALLS", filepath.Join(home, "calls"))
	t.Setenv("FAKE_FAIL", "")
	script := filepath.Join(home, "launchctl")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
printf '%s\n' "$1" >> "$FAKE_CALLS"
[ "$FAKE_FAIL" = "$1" ] && exit 5
case "$1" in
print) [ -f "$FAKE_STATE" ] && exit 0; exit 113 ;;
bootstrap) touch "$FAKE_STATE" ;;
bootout) rm "$FAKE_STATE" ;;
*) exit 1 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	old := runLaunchctl
	runLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, script, args...).CombinedOutput()
	}
	t.Cleanup(func() { runLaunchctl = old })
	return home
}

func TestLoginServiceLifecycle(t *testing.T) {
	home := fakeLaunchctl(t)
	project := filepath.Join(home, "project & <name>")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewLoginService(project, "")
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	other, err := NewLoginService(alias, "")
	if err != nil || other.Label != s.Label {
		t.Fatalf("symlink identity: %v, %v", other, err)
	}
	t.Setenv("OPENAI_API_KEY", "secret-must-not-be-persisted")
	ctx := context.Background()
	if err := s.Install(ctx, "/bin/echo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Install(ctx, "/bin/echo"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(os.Getenv("FAKE_CALLS"))
	if err != nil || strings.Count(string(calls), "bootstrap") != 1 {
		t.Fatalf("install not idempotent: %s, %v", calls, err)
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", s.Path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var plist map[string]any
	if err := json.Unmarshal(out, &plist); err != nil {
		t.Fatal(err)
	}
	wantArgs := []any{"/bin/echo", "watch", "--no-ui", "--no-worktrees", "--log-dir", s.LogDir}
	if !reflect.DeepEqual(plist["ProgramArguments"], wantArgs) || plist["WorkingDirectory"] != s.Directory {
		t.Fatalf("wrong launch configuration: %s", out)
	}
	if plist["RunAtLoad"] != true || plist["KeepAlive"] != true || plist["ThrottleInterval"] != float64(30) {
		t.Fatalf("missing retry/login policy: %s", out)
	}
	if !reflect.DeepEqual(plist["EnvironmentVariables"], map[string]any{"GREPAI_BACKGROUND": "1"}) {
		t.Fatalf("unexpected environment: %s", out)
	}
	info, err := os.Stat(s.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe plist permissions: %v, %v", info, err)
	}
	t.Setenv("FAKE_FAIL", "bootout")
	if err := s.Uninstall(ctx); err == nil {
		t.Fatal("expected unload failure")
	}
	if _, err := os.Stat(s.Path); err != nil {
		t.Fatal("unload failure removed registration", err)
	}
	t.Setenv("FAKE_FAIL", "print")
	if err := s.Uninstall(ctx); err == nil {
		t.Fatal("expected status failure, not silent deletion")
	}
	t.Setenv("FAKE_FAIL", "")
	if err := s.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatalf("registration remains: %v", err)
	}
	if _, err := os.Stat(project); err != nil {
		t.Fatal("uninstall changed project", err)
	}
	// launchctl bootout may return while the old process is still terminating.
	if err := os.WriteFile(os.Getenv("FAKE_STATE"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Install(ctx, "/bin/echo"); err == nil {
		t.Fatal("reinstall falsely succeeded while unregistered service was stopping")
	}
}

func TestLoginServiceBootstrapFailureAndWorkspaceIdentity(t *testing.T) {
	home := fakeLaunchctl(t)
	s, err := NewLoginService(home, "hum")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewLoginService(t.TempDir(), "hum")
	if err != nil || s.Label != other.Label {
		t.Fatalf("workspace identity depends on cwd: %v", err)
	}
	project, err := NewLoginService(home, "")
	if err != nil || project.Label == s.Label || project.LogDir == s.LogDir {
		t.Fatalf("project/workspace collision: %v", err)
	}
	t.Setenv("FAKE_FAIL", "bootstrap")
	if err := s.Install(context.Background(), "/bin/echo"); err == nil {
		t.Fatal("expected bootstrap failure")
	}
	if _, err := os.Stat(s.Path); err != nil {
		t.Fatal("registration must survive startup failure", err)
	}
	if !strings.Contains(string(s.plist("/bin/echo")), "<string>--workspace</string><string>hum</string>") {
		t.Fatal("workspace argument missing")
	}
}
