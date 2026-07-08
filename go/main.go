package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
)

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  unity-doc-corpus fetch --version <ver> [--destination <dir>] [--cache-root <dir>] [--force] [--resolve-only]")
	fmt.Fprintln(os.Stderr, "  unity-doc-corpus build --source <docs-root> --output <agent-output> [--workers N]")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "build":
		runBuild(os.Args[2:])
	case "fetch":
		runFetch(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runBuild(args []string) {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	source := fs.String("source", "", "Unity documentation language root containing Manual and ScriptReference.")
	output := fs.String("output", "", "Output directory for derived agent corpus.")
	workers := fs.Int("workers", 0, "Worker count for page transforms. Defaults to half of logical CPUs.")
	_ = fs.Parse(args)

	if *source == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "Usage: unity-doc-corpus build --source <docs-root> --output <agent-output> [--workers N]")
		os.Exit(2)
	}

	if err := build(*source, *output, *workers); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintln(os.Stderr, "error: sqlite query returned no rows")
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}
