package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/config"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublicationQuotaLegacyReplacementHasNoCredit(t *testing.T) {
	s := newTestStore(t)
	p := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	legacy := publishedState{Bundle: p.Bundle, MetadataBytes: p.MetadataBytes}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, bytes.Repeat([]byte(" "), 2<<20)...)
	if err = os.MkdirAll(filepath.Join(s.home, "published"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("published", p.Bundle.ArchiveSessionID+".json")
	if err = os.WriteFile(filepath.Join(s.home, path), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = config.WithPublicationComposition(s.home, func(c config.PublicationCompositionGuard) error {
		g, e := c.Storage(s.home)
		if e != nil {
			return e
		}
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() {
			if err := q.Close(); err != nil {
				t.Error(err)
			}
		}()
		info, e := q.home.Root.Lstat(path)
		if e != nil {
			return e
		}
		usage, e := q.usage()
		if e != nil {
			return e
		}
		if usage.charged != 0 {
			t.Fatal("legacy gained lifetime charge", usage)
		}
		extra, e := q.publishedAdditional(path, info, 1024)
		if e != nil {
			return e
		}
		if extra != int64(len(raw))+1024 {
			t.Fatal("legacy upgrade received capacity credit", extra)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationFirstEvidenceControlRefusesBeforeDirectoryCreation(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.home, "pending"), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(s.home, "pending", "occupied.json"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse disposable file leaves fewer than 64 KiB of charged capacity.
	if err = f.Truncate((durableStorageQuota - durableControlBytes/2) / 2); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = config.WithPublicationComposition(s.home, func(c config.PublicationCompositionGuard) error {
		g, e := c.Storage(s.home)
		if e != nil {
			return e
		}
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() {
			if err := q.Close(); err != nil {
				t.Error(err)
			}
		}()
		e = q.write("publication-evidence/synthetic/original.json", 3, func(w io.Writer) error { _, err := w.Write([]byte("{}\n")); return err })
		if !errors.Is(e, ErrDurableStorageCapacity) {
			t.Fatal("first control charge not enforced", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(s.home, "publication-evidence")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused write created directory or temp", err)
	}
}
