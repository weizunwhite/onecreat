// Command gen-bindings writes the frontend's AppBindings interface from the Go
// source that serves it. Run it from the desktop module root:
//
//	go generate ./...
//
// See desktop/internal/tsgen for why the interface is generated rather than
// typed out by hand.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"reasonix/desktop/internal/tsgen"
)

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	out, err := tsgen.Generate(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-bindings:", err)
		os.Exit(1)
	}
	path := filepath.Join(dir, tsgen.OutputFile)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen-bindings:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", path)
}
