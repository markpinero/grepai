package cli

import (
	"fmt"
	"os"

	"github.com/yoanbernabeu/grepai/config"
)

// inferWorkspaceScopeAt fills empty workspace/project from a path that sits
// inside a registered workspace project. Explicit workspace wins and does not
// infer project (so `grepai search --workspace hum` still searches all three).
func inferWorkspaceScopeAt(cwd string, workspace, project *string) (string, error) {
	if workspace != nil && *workspace != "" {
		return "", nil
	}

	match, err := config.MatchWorkspaceForPath(cwd)
	if err != nil {
		return "", err
	}
	if match == nil {
		return "", nil
	}

	if workspace != nil {
		*workspace = match.Name
	}

	inferredProject := false
	if project != nil && *project == "" && match.ProjectName != "" {
		*project = match.ProjectName
		inferredProject = true
	}

	msg := fmt.Sprintf("using workspace %s", match.Name)
	if inferredProject {
		msg += fmt.Sprintf(" / project %s", match.ProjectName)
	}
	return msg + " (cwd)", nil
}

func applyInferredWorkspace(workspace *string, project *string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	msg, err := inferWorkspaceScopeAt(cwd, workspace, project)
	if err != nil {
		return err
	}
	if msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	return nil
}
