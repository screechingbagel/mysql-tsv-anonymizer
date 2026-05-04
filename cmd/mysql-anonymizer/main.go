// Command mysql-anonymizer rewrites configured columns of a mysqlsh
// util.dumpInstance directory and emits a sibling clean directory.
// See docs/superpowers/specs/2026-05-03-mysql-anonymizer-design.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

type opts struct {
	InDir      string
	OutDir     string
	ConfigPath string
	Seed       uint64
	Workers    int
}

func parseFlags(args []string) (opts, error) {
	fs := flag.NewFlagSet("mysql-anonymizer", flag.ContinueOnError)
	var o opts

	fs.StringVar(&o.InDir, "in", "", "input dump-dir")
	fs.StringVar(&o.OutDir, "out", "", "output clean-dir")
	fs.StringVar(&o.ConfigPath, "c", "", "YAML config path")
	fs.Uint64Var(&o.Seed, "seed", 0, "job seed")
	fs.IntVar(&o.Workers, "j", runtime.NumCPU(), "worker count")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, err // bubble up so main can exit 0
		}
		return o, err
	}

	// Check if seed was explicitly set
	seedSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seedSet = true
		}
	})

	switch {
	case o.InDir == "":
		return o, errors.New("--in is required")
	case o.OutDir == "":
		return o, errors.New("--out is required")
	case o.ConfigPath == "":
		return o, errors.New("-c is required")
	case !seedSet:
		return o, errors.New("--seed is required (no implicit default)")
	case o.Workers <= 0:
		return o, fmt.Errorf("-j must be > 0 (got %d)", o.Workers)
	}
	return o, nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func run(ctx context.Context, o opts) error {
	_ = ctx
	_ = o
	return errors.New("not implemented")
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, cancel := signalContext()
	defer cancel()

	if err := run(ctx, o); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
