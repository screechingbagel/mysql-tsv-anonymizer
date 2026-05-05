// Command mysql-anonymizer rewrites configured columns of a mysqlsh
// util.dumpInstance directory and emits a sibling clean directory.
// See docs/superpowers/specs/2026-05-03-mysql-anonymizer-design.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/config"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"
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
	// 1. Manifest.
	manifest, err := dump.WalkManifest(o.InDir)
	if err != nil {
		return err
	}
	if !manifest.HasDoneMarker {
		return fmt.Errorf("--in lacks @.done.json (the dump is incomplete)")
	}

	// 2. Sanity-parse @.json.
	if manifest.InstanceMetaPath == "" {
		return fmt.Errorf("--in lacks @.json")
	}
	instMeta, err := dump.ReadInstanceMeta(manifest.InstanceMetaPath)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(instMeta.Version, "2.") {
		return fmt.Errorf("dump version %q is not supported (only 2.x is supported)", instMeta.Version)
	}

	// 3. Load + bootstrap-validate config.
	rc, err := config.LoadRaw(o.ConfigPath)
	if err != nil {
		return err
	}
	bootF := faker.New(rand.NewPCG(0xdeadbeef, 0xcafebabe))
	if _, err := rc.Compile(bootF); err != nil {
		return err
	}

	// 4. Strict validate.
	schemas, err := Validate(rc, manifest)
	if err != nil {
		return err
	}

	// 5. --out must not exist or be empty.
	if entries, err := os.ReadDir(o.OutDir); err == nil {
		if len(entries) > 0 {
			return fmt.Errorf("--out exists and is non-empty: %s", o.OutDir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// 6. Copy pass.
	configured := make(map[string]struct{}, len(schemas))
	for k := range schemas {
		configured[k] = struct{}{}
	}
	if err := PreparePassthrough(manifest, configured, o.OutDir); err != nil {
		return err
	}

	// 7. Build job list.
	var jobs []job
	for k := range schemas {
		for _, c := range manifest.Tables[k].Chunks {
			jobs = append(jobs, job{tableKey: k, schema: schemas[k], chunk: c})
		}
	}

	// 8. Run pool.
	if err := RunPool(ctx, jobs, rc, schemas, o.Seed, o.OutDir, o.Workers); err != nil {
		return err
	}

	// 9. Finalize: copy @.done.json LAST.
	// @.done.json is NOT in manifest.PassthroughFiles — WalkManifest sets
	// HasDoneMarker and skips it. Use manifest.Root directly.
	return linkOrCopy(filepath.Join(manifest.Root, "@.done.json"), filepath.Join(o.OutDir, "@.done.json"))
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
