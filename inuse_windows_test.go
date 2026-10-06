//go:build windows

package codexcli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirstFileInUse(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "node_modules", "@openai", "codex-win32-x64", "bin")
	mustMkdir(t, nested)
	idle := filepath.Join(root, "package.json")
	held := filepath.Join(nested, "codex.exe")
	mustWrite(t, idle, "{}", 0o644)
	mustWrite(t, held, "", 0o644)
	missing := filepath.Join(root, "codex.ps1")

	if got, err := firstFileInUse([]string{root, missing}); got != "" || err != nil {
		t.Fatalf("idle tree: firstFileInUse = %q, %v; want nothing held", got, err)
	}

	// os.Open shares read and write but not delete, like an editor or a
	// scanner holding the file: npm's rename of the tree fails with EBUSY.
	f, err := os.Open(held)
	if err != nil {
		t.Fatal(err)
	}
	got, err := firstFileInUse([]string{missing, root})
	f.Close()
	if got != held || err != nil {
		t.Errorf("held without delete sharing: firstFileInUse = %q, %v; want %q", got, err, held)
	}

	// A running image refuses write access: this test binary is one.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := firstFileInUse([]string{self}); got != self || err != nil {
		t.Errorf("running image: firstFileInUse = %q, %v; want %q", got, err, self)
	}
}

func TestEnvKeyIsCaseInsensitiveOnWindows(t *testing.T) {
	overrides := map[string]string{"Path": `C:\only`}
	if got := envValue(overrides, "PATH"); got != `C:\only` {
		t.Errorf("envValue = %q, want the Path override", got)
	}
	merged := withPathPrefix(overrides, `C:\npm`)
	if len(merged) != 1 || merged["Path"] != `C:\npm;C:\only` {
		t.Errorf("withPathPrefix = %v, want the one Path override prefixed", merged)
	}
}
