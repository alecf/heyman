package main

import (
	"fmt"
	"os"

	"github.com/alecf/heyman/internal/cli"
)

var (
	// Injected at build time
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := cli.Execute(version, commit, date); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
