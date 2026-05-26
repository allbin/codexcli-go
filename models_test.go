package codexcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// isolateHome scrubs the env vars ListModels consults so the test sees
// only the fixture we point it at — no leaking into the real $HOME/.codex
// on a developer machine.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", t.TempDir())
}

func writeCache(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}

func TestListModels_HappyPath(t *testing.T) {
	isolateHome(t)

	fixture, err := os.ReadFile(filepath.Join("testdata", "models_cache.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	home := t.TempDir()
	writeCache(t, home, string(fixture))

	models, err := ListModels(context.Background(), WithCodexHome(home))
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 6 {
		t.Fatalf("expected 6 models, got %d", len(models))
	}

	wantSlugs := []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.2", "codex-auto-review"}
	for i, want := range wantSlugs {
		if models[i].Slug != want {
			t.Errorf("model[%d].Slug = %q, want %q", i, models[i].Slug, want)
		}
	}

	gpt55 := models[0]
	if gpt55.DisplayName != "GPT-5.5" {
		t.Errorf("DisplayName = %q", gpt55.DisplayName)
	}
	if gpt55.Visibility != VisibilityList {
		t.Errorf("Visibility = %q, want %q", gpt55.Visibility, VisibilityList)
	}
	if !gpt55.SupportedInAPI {
		t.Errorf("SupportedInAPI = false, want true")
	}
	if gpt55.Priority != 9 {
		t.Errorf("Priority = %d, want 9", gpt55.Priority)
	}
	if len(gpt55.SupportedReasoningLevels) != 4 {
		t.Errorf("SupportedReasoningLevels len = %d, want 4", len(gpt55.SupportedReasoningLevels))
	}
	if gpt55.SupportedReasoningLevels[0].Effort != "low" {
		t.Errorf("SupportedReasoningLevels[0].Effort = %q", gpt55.SupportedReasoningLevels[0].Effort)
	}
	if gpt55.AvailabilityNUX == nil || gpt55.AvailabilityNUX.Message == "" {
		t.Errorf("AvailabilityNUX missing")
	}
	if gpt55.TruncationPolicy == nil || gpt55.TruncationPolicy.Mode != "tokens" {
		t.Errorf("TruncationPolicy = %+v", gpt55.TruncationPolicy)
	}

	// last entry has visibility "hide"
	if models[5].Visibility != VisibilityHide {
		t.Errorf("codex-auto-review Visibility = %q, want %q", models[5].Visibility, VisibilityHide)
	}

	// gpt-5.3-codex carries a non-null upgrade pointer
	for _, m := range models {
		if m.Slug == "gpt-5.3-codex" {
			if m.Upgrade == nil || m.Upgrade.Model == "" {
				t.Errorf("gpt-5.3-codex Upgrade missing or empty: %+v", m.Upgrade)
			}
		}
	}
}

func TestListModels_MissingFile(t *testing.T) {
	isolateHome(t)
	home := t.TempDir() // empty — no models_cache.json

	_, err := ListModels(context.Background(), WithCodexHome(home))
	if !errors.Is(err, ErrModelsCacheUnavailable) {
		t.Fatalf("want ErrModelsCacheUnavailable, got %v", err)
	}
}

func TestListModels_MalformedJSON(t *testing.T) {
	isolateHome(t)
	home := t.TempDir()
	writeCache(t, home, "{not json")

	_, err := ListModels(context.Background(), WithCodexHome(home))
	if err == nil {
		t.Fatal("want parse error, got nil")
	}
	if errors.Is(err, ErrModelsCacheUnavailable) {
		t.Fatalf("parse error should not match ErrModelsCacheUnavailable: %v", err)
	}
}

func TestListModels_CodexHomeEnvOverridesHomeFallback(t *testing.T) {
	isolateHome(t)

	// HOME/.codex points at an empty dir (would yield ErrModelsCacheUnavailable
	// if consulted). CODEX_HOME points at a dir with a real fixture — env
	// should win.
	envHome := t.TempDir()
	writeCache(t, envHome, `{"models":[{"slug":"only-from-env","visibility":"list"}]}`)
	t.Setenv("CODEX_HOME", envHome)

	models, err := ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].Slug != "only-from-env" {
		t.Fatalf("expected env-fixture model, got %+v", models)
	}
}

func TestListModels_EmptyModelsArray(t *testing.T) {
	isolateHome(t)
	home := t.TempDir()
	writeCache(t, home, `{"models":[]}`)

	models, err := ListModels(context.Background(), WithCodexHome(home))
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if models == nil {
		t.Fatal("want non-nil empty slice, got nil")
	}
	if len(models) != 0 {
		t.Fatalf("want 0 models, got %d", len(models))
	}
}

func TestClientListModels_UsesClientDefaults(t *testing.T) {
	isolateHome(t)
	home := t.TempDir()
	writeCache(t, home, `{"models":[{"slug":"from-client-default","visibility":"list"}]}`)

	c := New(WithCodexHome(home))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].Slug != "from-client-default" {
		t.Fatalf("expected client-default fixture model, got %+v", models)
	}
}
