package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yoanbernabeu/grepai/config"
)

func TestInferWorkspaceScopeAt(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := setTestHomeDirCLI(t, tmpDir)
	defer cleanup()

	storefront := filepath.Join(tmpDir, "HUM", "hum-storefront")
	api := filepath.Join(tmpDir, "HUM", "hum-api")
	other := filepath.Join(tmpDir, "other")
	if err := os.MkdirAll(filepath.Join(storefront, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(api, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultWorkspaceConfig()
	if err := cfg.AddWorkspace(config.Workspace{
		Name:  "hum",
		Store: config.StoreConfig{Backend: "qdrant"},
		Projects: []config.ProjectEntry{
			{Name: "hum-storefront", Path: storefront},
			{Name: "hum-api", Path: api},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveWorkspaceConfig(cfg); err != nil {
		t.Fatal(err)
	}

	t.Run("cwd infers workspace and project", func(t *testing.T) {
		ws, project := "", ""
		msg, err := inferWorkspaceScopeAt(filepath.Join(storefront, "src"), &ws, &project)
		if err != nil {
			t.Fatalf("infer: %v", err)
		}
		if ws != "hum" {
			t.Errorf("workspace = %q, want hum", ws)
		}
		if project != "hum-storefront" {
			t.Errorf("project = %q, want hum-storefront", project)
		}
		if msg != "using workspace hum / project hum-storefront (cwd)" {
			t.Errorf("msg = %q", msg)
		}
	})

	t.Run("explicit workspace does not infer project", func(t *testing.T) {
		ws, project := "hum", ""
		msg, err := inferWorkspaceScopeAt(storefront, &ws, &project)
		if err != nil {
			t.Fatalf("infer: %v", err)
		}
		if project != "" {
			t.Errorf("project = %q, want empty (whole workspace)", project)
		}
		if msg != "" {
			t.Errorf("msg = %q, want empty when flags already set workspace", msg)
		}
	})

	t.Run("unrelated cwd does not infer", func(t *testing.T) {
		ws, project := "", ""
		msg, err := inferWorkspaceScopeAt(other, &ws, &project)
		if err != nil {
			t.Fatalf("infer: %v", err)
		}
		if ws != "" || project != "" || msg != "" {
			t.Errorf("got ws=%q project=%q msg=%q, want all empty", ws, project, msg)
		}
	})

	t.Run("explicit project kept when inferring workspace", func(t *testing.T) {
		ws, project := "", "hum-api"
		msg, err := inferWorkspaceScopeAt(storefront, &ws, &project)
		if err != nil {
			t.Fatalf("infer: %v", err)
		}
		if ws != "hum" {
			t.Errorf("workspace = %q, want hum", ws)
		}
		if project != "hum-api" {
			t.Errorf("project = %q, want explicit hum-api", project)
		}
		if msg != "using workspace hum (cwd)" {
			t.Errorf("msg = %q", msg)
		}
	})
}
