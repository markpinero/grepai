package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yoanbernabeu/grepai/config"
	"github.com/yoanbernabeu/grepai/daemon"
)

const watchLogDirHintFileName = "watch-log-dir"

func workspaceLogDirHintPath(workspace string) (string, error) {
	dir, err := config.GetGlobalConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("workspace-%x.watch-log-dir", sha256.Sum256([]byte(workspace)))), nil
}

func readWorkspaceLogDirHint(workspace string) (string, error) {
	path, err := workspaceLogDirHintPath(workspace)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

func saveWorkspaceLogDirHint(workspace, logDir string) error {
	path, err := workspaceLogDirHintPath(workspace)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	absolute, err := filepath.Abs(logDir)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(absolute+"\n"), 0600)
}

func watchLogDirHintPath(projectRoot string) string {
	return filepath.Join(config.GetConfigDir(projectRoot), watchLogDirHintFileName)
}

func readWatchLogDirHint(projectRoot string) (string, error) {
	data, err := os.ReadFile(watchLogDirHintPath(projectRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func clearWatchLogDirHint(projectRoot string) error {
	err := os.Remove(watchLogDirHintPath(projectRoot))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func saveWatchLogDirHint(projectRoot, logDir string) error {
	if projectRoot == "" {
		return nil
	}
	rawLogDir := strings.TrimSpace(logDir)
	if rawLogDir == "" {
		return clearWatchLogDirHint(projectRoot)
	}
	cleanLogDir := filepath.Clean(rawLogDir)
	if !filepath.IsAbs(cleanLogDir) {
		if absLogDir, err := filepath.Abs(cleanLogDir); err == nil {
			cleanLogDir = absLogDir
		}
	}

	defaultLogDir, err := daemon.GetDefaultLogDir()
	if err == nil && filepath.Clean(defaultLogDir) == cleanLogDir {
		return clearWatchLogDirHint(projectRoot)
	}

	cfgDir := config.GetConfigDir(projectRoot)
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(watchLogDirHintPath(projectRoot), []byte(cleanLogDir+"\n"), 0600)
}
