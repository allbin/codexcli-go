package codexcli

import (
	"os"
	"path/filepath"
	"testing"
)

// writeShimLayout builds a fake npm install: a shim file plus the CLI
// package at pkgRel (relative to the shim's directory), with the given
// package.json content and entry script.
func writeShimLayout(t *testing.T, shimName, pkgRel, pkgJSON, entryName string) (shimPath, entryPath string) {
	t.Helper()
	root := t.TempDir()
	shimPath = filepath.Join(root, shimName)
	if err := os.WriteFile(shimPath, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, pkgRel)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if pkgJSON != "" {
		if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(pkgJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if entryName != "" {
		entryPath = filepath.Join(pkgDir, filepath.FromSlash(entryName))
		if err := os.MkdirAll(filepath.Dir(entryPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(entryPath, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return shimPath, entryPath
}

const codexPkgJSON = `{"name":"@openai/codex","bin":{"codex":"bin/codex.js"}}`

func TestFindShimEntryJS_GlobalNPMLayout(t *testing.T) {
	shim, entry := writeShimLayout(t, "codex.cmd",
		filepath.Join("node_modules", "@openai", "codex"), codexPkgJSON, "bin/codex.js")
	got, ok := findShimEntryJS(shim)
	if !ok || got != entry {
		t.Fatalf("findShimEntryJS = %q, %v; want %q, true", got, ok, entry)
	}
}

func TestFindShimEntryJS_UnixPrefixLayout(t *testing.T) {
	// prefix/bin/codex.cmd with the package under prefix/lib/node_modules.
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(binDir, "codex.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "lib", "node_modules", "@openai", "codex")
	if err := os.MkdirAll(filepath.Join(pkgDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(codexPkgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(pkgDir, "bin", "codex.js")
	if err := os.WriteFile(entry, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := findShimEntryJS(shim)
	if !ok || got != entry {
		t.Fatalf("findShimEntryJS = %q, %v; want %q, true", got, ok, entry)
	}
}

func TestFindShimEntryJS_BinAsString(t *testing.T) {
	shim, entry := writeShimLayout(t, "codex.cmd",
		filepath.Join("node_modules", "@openai", "codex"),
		`{"name":"@openai/codex","bin":"main.js"}`, "main.js")
	got, ok := findShimEntryJS(shim)
	if !ok || got != entry {
		t.Fatalf("findShimEntryJS = %q, %v; want %q, true", got, ok, entry)
	}
}

func TestFindShimEntryJS_MissingBinFallsBack(t *testing.T) {
	shim, entry := writeShimLayout(t, "codex.cmd",
		filepath.Join("node_modules", "@openai", "codex"),
		`{"name":"@openai/codex"}`, "bin/codex.js")
	got, ok := findShimEntryJS(shim)
	if !ok || got != entry {
		t.Fatalf("findShimEntryJS = %q, %v; want %q, true", got, ok, entry)
	}
}

func TestFindShimEntryJS_NoBypass(t *testing.T) {
	pkgRel := filepath.Join("node_modules", "@openai", "codex")
	cases := []struct {
		name              string
		shimName, pkgJSON string
		entry             string
	}{
		{"foreign package", "codex.cmd", `{"name":"someone-else/codex"}`, "bin/codex.js"},
		{"missing package.json", "codex.cmd", "", "bin/codex.js"},
		{"missing entry script", "codex.cmd", codexPkgJSON, ""},
		{"not a shim extension", "codex.exe", codexPkgJSON, "bin/codex.js"},
		{"unparseable package.json", "codex.cmd", `{not json`, "bin/codex.js"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shim, _ := writeShimLayout(t, tc.shimName, pkgRel, tc.pkgJSON, tc.entry)
			if got, ok := findShimEntryJS(shim); ok {
				t.Fatalf("findShimEntryJS = %q, true; want no bypass", got)
			}
		})
	}
}
