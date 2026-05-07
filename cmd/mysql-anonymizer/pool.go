package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"text/template"

	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/anon"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/config"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/dump"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/faker"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/idx"
	"github.com/screechingbagel/mysql-tsv-anonymizer/internal/tsv"
	lzstd "github.com/screechingbagel/mysql-tsv-anonymizer/internal/zstd"
)

type job struct {
	tableKey string
	schema   *tableSchema
	chunk    dump.ChunkEntry
	size     uint64 // compressed bytes of the input chunk; used for progress reporting
}

// RunPool runs nWorkers goroutines processing jobs. Returns the first error
// encountered (others are observed but not returned).
func RunPool(
	ctx context.Context,
	jobs []job,
	rc *config.RawConfig,
	schemas map[string]*tableSchema,
	jobSeed uint64,
	outDir string,
	nWorkers int,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobCh := make(chan job)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	record := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel()
	}

	for range nWorkers {
		wg.Go(func() {
			for j := range jobCh {
				if ctx.Err() != nil {
					return
				}
				if err := processChunk(ctx, j, rc, jobSeed, outDir); err != nil {
					record(fmt.Errorf("chunk %s@@%d: %w", j.tableKey, j.chunk.Index, err))
					return
				}
			}
		})
	}
	go func() {
		defer close(jobCh)
		for _, j := range jobs {
			select {
			case jobCh <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// deriveSeed mixes (jobSeed, tableKey, chunkIdx) into a (hi, lo) pair for PCG.
func deriveSeed(jobSeed uint64, tableKey string, chunkIdx int) (uint64, uint64) {
	h := fnv.New64a()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], jobSeed)
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(tableKey))
	binary.BigEndian.PutUint64(buf[:], uint64(chunkIdx))
	_, _ = h.Write(buf[:])
	hi := h.Sum64()
	// Stir again for the second word.
	_, _ = h.Write([]byte{0x5a})
	lo := h.Sum64()
	return hi, lo
}

// processChunk handles one (table, chunk) job: derive RNG, compile templates,
// build slot list, stream-rewrite the chunk, atomic-rename outputs.
func processChunk(ctx context.Context, j job, rc *config.RawConfig, jobSeed uint64, outDir string) (err error) {
	hi, lo := deriveSeed(jobSeed, j.tableKey, j.chunk.Index)
	f := faker.New(rand.NewPCG(hi, lo))
	f.SetInvoiceBase(uint64(j.chunk.Index) * faker.InvoiceStride)
	cc, err := rc.Compile(f)
	if err != nil {
		return fmt.Errorf("compile config: %w", err)
	}
	colRules := cc.Rules[j.schema.ConfigTable]
	slots := make([]*template.Template, len(j.schema.Columns))
	for i, col := range j.schema.Columns {
		slots[i] = colRules[col]
	}

	inF, err := os.Open(j.chunk.DataPath)
	if err != nil {
		return err
	}
	defer inF.Close()
	zr, err := lzstd.NewReader(inF)
	if err != nil {
		return err
	}
	defer zr.Close()

	dstData := filepath.Join(outDir, filepath.Base(j.chunk.DataPath))
	dstIdx := filepath.Join(outDir, filepath.Base(j.chunk.IdxPath))
	tmpData := dstData + ".tmp"
	tmpIdx := dstIdx + ".tmp"

	outF, err := os.Create(tmpData)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmpData)
			_ = os.Remove(tmpIdx)
		}
	}()
	defer outF.Close()

	idxF, err := os.Create(tmpIdx)
	if err != nil {
		return err
	}
	defer idxF.Close()

	zw, err := lzstd.NewWriter(outF)
	if err != nil {
		return err
	}
	tw := tsv.NewWriter(zw)
	tr := tsv.NewReader(zr)

	hook := func(_ int64) error { return ctx.Err() }
	if err := anon.ProcessAllWithRowHook(tr, tw, slots, hook); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := outF.Sync(); err != nil {
		return err
	}
	if err := idx.Write(idxF, tw.BytesWritten()); err != nil {
		return err
	}
	if err := idxF.Sync(); err != nil {
		return err
	}
	if err := os.Rename(tmpData, dstData); err != nil {
		return err
	}
	if err := os.Rename(tmpIdx, dstIdx); err != nil {
		return err
	}
	return nil
}
