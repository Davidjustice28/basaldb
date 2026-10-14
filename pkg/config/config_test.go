package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"basaldb/pkg/engine"
)

// useTempConfig points $BASAL_CONFIG at a fresh path for the test.
func useTempConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	t.Setenv("BASAL_CONFIG", path)
	return path
}

func TestSetLoadRoundTrip(t *testing.T) {
	useTempConfig(t)

	if c, err := Load(); err != nil || c != (Config{}) {
		t.Fatalf("Load on a missing file = %+v, %v; want empty, nil", c, err)
	}
	if err := Set(KeyFile, "demo.bsl"); err != nil {
		t.Fatalf("Set(file): %v", err)
	}
	if err := Set(KeyBranch, "scratch"); err != nil {
		t.Fatalf("Set(branch): %v", err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.File != "demo.bsl" || c.Branch != "scratch" {
		t.Errorf("Load = %+v, want demo.bsl/scratch", c)
	}
	if v, err := Get(KeyFile); err != nil || v != "demo.bsl" {
		t.Errorf("Get(file) = %q, %v", v, err)
	}
	if err := Unset(KeyBranch); err != nil {
		t.Fatalf("Unset(branch): %v", err)
	}
	c, err = Load()
	if err != nil || c.File != "demo.bsl" || c.Branch != "" {
		t.Errorf("after Unset: %+v, %v", c, err)
	}
}

func TestPathHonoursEnvOverride(t *testing.T) {
	path := useTempConfig(t)
	got, err := Path()
	if err != nil || got != path {
		t.Errorf("Path() = %q, %v; want %q", got, err, path)
	}
}

func TestSetRejectsBadKeysAndValues(t *testing.T) {
	useTempConfig(t)
	if err := Set("flie", "x"); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("Set(unknown key) = %v, want unknown key error", err)
	}
	if err := Set(KeyFile, "   "); err == nil {
		t.Error("Set(empty value) succeeded, want error")
	}
	if _, err := Get(KeyBranch); err == nil {
		t.Error("Get(unset key) succeeded, want error")
	}
	if err := Unset("nope"); err == nil {
		t.Error("Unset(unknown key) succeeded, want error")
	}
}

func TestLoadParsesRealisticFile(t *testing.T) {
	path := useTempConfig(t)
	content := "# basal CLI config\n[cli]\nfile = /data/tenants.bsl\nbranch = feature\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.File != "/data/tenants.bsl" || c.Branch != "feature" {
		t.Errorf("Load = %+v", c)
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	path := useTempConfig(t)
	for name, content := range map[string]string{
		"missing equals": "file demo.bsl\n",
		"unknown key":    "[cli]\nflie = demo.bsl\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Errorf("Load(%s) succeeded, want error", name)
		}
	}
}

func TestResolvePrecedence(t *testing.T) {
	useTempConfig(t)

	// Nothing set: fallbacks.
	if f, err := ResolveFile(""); err != nil || f != FallbackFile {
		t.Errorf("ResolveFile(\"\") = %q, %v; want %q", f, err, FallbackFile)
	}
	if b, err := ResolveBranch(""); err != nil || b != engine.DefaultBranch {
		t.Errorf("ResolveBranch(\"\") = %q, %v; want %q", b, err, engine.DefaultBranch)
	}

	// Config applies when the flag is empty.
	if err := Set(KeyFile, "cfg.bsl"); err != nil {
		t.Fatal(err)
	}
	if err := Set(KeyBranch, "dev"); err != nil {
		t.Fatal(err)
	}
	if f, err := ResolveFile(""); err != nil || f != "cfg.bsl" {
		t.Errorf("ResolveFile(\"\") = %q, %v; want cfg.bsl", f, err)
	}
	if b, err := ResolveBranch(""); err != nil || b != "dev" {
		t.Errorf("ResolveBranch(\"\") = %q, %v; want dev", b, err)
	}

	// The flag always wins over the config.
	if f, err := ResolveFile("once.bsl"); err != nil || f != "once.bsl" {
		t.Errorf("ResolveFile(once.bsl) = %q, %v", f, err)
	}
	if b, err := ResolveBranch("main"); err != nil || b != "main" {
		t.Errorf("ResolveBranch(main) = %q, %v", b, err)
	}
}

func TestResolveSurfacesConfigErrors(t *testing.T) {
	path := useTempConfig(t)
	if err := os.WriteFile(path, []byte("not ini\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveFile(""); err == nil {
		t.Error("ResolveFile with a corrupt config succeeded, want error")
	}
	// A flag short-circuits before the config is even read.
	if f, err := ResolveFile("ok.bsl"); err != nil || f != "ok.bsl" {
		t.Errorf("ResolveFile(ok.bsl) = %q, %v", f, err)
	}
}
