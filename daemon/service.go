package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"

	"github.com/yoanbernabeu/grepai/git"
)

var runLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
}

// LoginService runs a foreground watcher under macOS launchd, not a detached child.
type LoginService struct {
	Label, Path, LogDir, Directory string
	Workspace                      string
}

func NewLoginService(directory, workspace string) (*LoginService, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("login services are currently supported only on macOS")
	}
	directory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	logDir, err := GetDefaultLogDir()
	if err != nil {
		return nil, err
	}
	identity := "project:" + directory
	if workspace != "" {
		identity = "workspace:" + workspace
		directory = home
	}
	hash := sha256.Sum256([]byte(identity))
	label := fmt.Sprintf("io.grepai.watch.%x", hash[:12])
	if workspace == "" {
		logDir = filepath.Join(logDir, label)
	}
	return &LoginService{
		Label: label, Path: filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		Directory: directory, LogDir: logDir, Workspace: workspace,
	}, nil
}

func (s *LoginService) plist(executable string) []byte {
	escape := func(value string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(value))
		return b.String()
	}
	args := []string{executable, "watch", "--no-ui", "--no-worktrees", "--log-dir", s.LogDir}
	if s.Workspace != "" {
		args = append(args, "--workspace", s.Workspace)
	}
	var arguments bytes.Buffer
	for _, arg := range args {
		fmt.Fprintf(&arguments, "<string>%s</string>", escape(arg))
	}
	logPath := filepath.Join(s.LogDir, logFileName)
	if s.Workspace != "" {
		logPath = GetWorkspaceLogFile(s.LogDir, s.Workspace)
	} else if info, err := git.Detect(s.Directory); err == nil && info.WorktreeID != "" {
		logPath = GetWorktreeLogFile(s.LogDir, info.WorktreeID)
	}
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array>%s</array>
<key>WorkingDirectory</key><string>%s</string>
<key>EnvironmentVariables</key><dict><key>GREPAI_BACKGROUND</key><string>1</string></dict>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>30</integer>
<key>ExitTimeOut</key><integer>120</integer>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, escape(s.Label), arguments.String(), escape(s.Directory), escape(logPath), escape(logPath))
}

func launchDomain() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return "gui/" + u.Uid, nil
}

func (s *LoginService) Status(ctx context.Context) (string, error) {
	domain, err := launchDomain()
	if err != nil {
		return "", err
	}
	out, err := runLaunchctl(ctx, "print", domain+"/"+s.Label)
	return string(out), err
}

func (s *LoginService) Loaded(ctx context.Context) (bool, error) {
	out, err := s.Status(ctx)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 113 {
		return false, nil
	}
	return false, fmt.Errorf("launchctl print: %w: %s", err, out)
}

// Install is idempotent: an already loaded service is left running.
// No shell environment (in particular API keys) is persisted in the plist.
func (s *LoginService) Install(ctx context.Context, executable string) error {
	loaded, err := s.Loaded(ctx)
	if err != nil {
		return err
	}
	if loaded {
		if _, err := os.Stat(s.Path); err != nil {
			return fmt.Errorf("service is still loaded without its registration; wait for shutdown before reinstalling: %w", err)
		}
		return nil
	}
	if !filepath.IsAbs(executable) {
		return fmt.Errorf("service executable must be an absolute path")
	}
	if _, err := os.Stat(executable); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.LogDir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".grepai-*.plist")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(s.plist(executable)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.Path); err != nil {
		return err
	}
	domain, err := launchDomain()
	if err != nil {
		return err
	}
	out, err := runLaunchctl(ctx, "bootstrap", domain, s.Path)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s (plist retained at %s)", err, out, s.Path)
	}
	return nil
}

func (s *LoginService) Uninstall(ctx context.Context) error {
	loaded, err := s.Loaded(ctx)
	if err != nil {
		return err
	}
	if loaded {
		domain, err := launchDomain()
		if err != nil {
			return err
		}
		out, err := runLaunchctl(ctx, "bootout", domain+"/"+s.Label)
		if err != nil {
			return fmt.Errorf("launchctl bootout: %w: %s", err, out)
		}
	}
	if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
