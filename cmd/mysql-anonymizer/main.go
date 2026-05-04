package main

import (
	"context"
	"errors"
	"flag"
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
		return opts{}, err
	}

	// Check required flags
	if o.InDir == "" {
		return opts{}, errors.New("flag --in is required")
	}
	if o.OutDir == "" {
		return opts{}, errors.New("flag --out is required")
	}
	if o.ConfigPath == "" {
		return opts{}, errors.New("flag -c is required")
	}

	// Check if seed was explicitly set
	seedSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seedSet = true
		}
	})
	if !seedSet {
		return opts{}, errors.New("flag --seed is required")
	}

	return o, nil
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func run(ctx context.Context, o opts) error {
	return errors.New("not implemented")
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		os.Stderr.WriteString("Error: " + err.Error() + "\n")
		os.Exit(1)
	}

	ctx, cancel := signalContext()
	defer cancel()

	if err := run(ctx, o); err != nil {
		os.Stderr.WriteString("Error: " + err.Error() + "\n")
		os.Exit(1)
	}
}
