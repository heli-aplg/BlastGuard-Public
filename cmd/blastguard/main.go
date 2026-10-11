// Command BlastGuard analyzes the impact of deleting one PostgreSQL row. It is
// strictly read-only: it never executes the scripts it generates.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"blastguard/internal/bootstrap"
	"blastguard/internal/config"
	"blastguard/internal/tui"
)

var version = "0.1.0"

func main() {
	cfgPath := flag.String("config", "", "config file (default: ./blastguard.yaml, then <source>/blastguard.yaml)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("BlastGuard", version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "blastguard:", err)
		os.Exit(2)
	}

	ctx := context.Background()
	boot, err := bootstrap.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "blastguard:", err)
		os.Exit(1)
	}
	out, err := tui.Run(tui.New(cfg, boot.RO, boot.BC, boot.Meta))
	boot.Close() // before os.Exit, which skips defers
	if out != "" {
		fmt.Println(out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "blastguard:", err)
		os.Exit(1)
	}
}
