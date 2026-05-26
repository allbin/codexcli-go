// Command genschema runs `codex app-server generate-json-schema --out
// DIR` to refresh the raw JSON Schema bundle used by the schema package.
//
// Invoke via `go generate ./...` after upgrading the installed codex
// CLI. Diff the refreshed output against schema/types.go to spot new
// fields, methods, or enum variants that need hand-written type updates.
// See README.md "Updating the protocol" for the full process.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	var (
		binary = flag.String("bin", "codex", "codex CLI binary")
		out    = flag.String("out", "schema_v2_raw", "output directory for the raw schema bundle")
	)
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *out, err)
	}
	abs, err := filepath.Abs(*out)
	if err != nil {
		log.Fatalf("abs %s: %v", *out, err)
	}

	cmd := exec.Command(*binary, "app-server", "generate-json-schema", "--out", abs)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%s app-server generate-json-schema: %v", *binary, err)
	}
	fmt.Printf("wrote codex JSON schemas to %s\n", abs)
}
