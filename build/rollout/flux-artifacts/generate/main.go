package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/fredrir/infra/internal/fluxartifacts"
)

func main() {
	check := flag.Bool("check", false, "Reject stale generated overlays")
	root := flag.String("root", "../../../..", "Repository root")
	flag.Parse()
	if err := fluxartifacts.Run(*root, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
