package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Cancel a real context at the second non-empty census batch checkpoint.
type censusCancellation struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *censusCancellation) Err() error {
	c.checks++
	if c.checks == 5 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestDurableCensusCancellationRetainsPartialObligations(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 257 {
		dir := filepath.Join(s.home, "publication-evidence", fmt.Sprintf("owed-%03d", i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "original"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if all, err := s.DurableStorageObligations(); err != nil || len(all) != 257 {
		t.Fatal("warm census", len(all), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &censusCancellation{Context: ctx, cancel: cancel}
	scoped, closeScope := s.WithReadBudget(observed, nil)
	partial, err := scoped.DurableStorageObligations()
	closeScope()
	if !errors.Is(err, context.Canceled) || len(partial) == 0 || len(partial) >= 257 {
		t.Fatalf("partial census certified complete/empty: count=%d err=%v checks=%d", len(partial), err, observed.checks)
	}
	all, err := s.DurableStorageObligations()
	if err != nil || len(all) != 257 || s.durableInspection.configLoads != 1 || s.durableInspection.scans != 1 {
		t.Fatalf("cancel affected subsequent complete census: count=%d err=%v configs=%d scans=%d", len(all), err, s.durableInspection.configLoads, s.durableInspection.scans)
	}
}
