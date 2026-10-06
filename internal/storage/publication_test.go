package storage_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type publicationFaultStore struct {
	*storagetest.MemoryStore
	puts            int
	stats           int
	failKey         string
	uncertain       bool
	corruptMetadata bool
	metadataKey     string
}

func (s *publicationFaultStore) Put(ctx context.Context, key string, data []byte) error {
	s.puts++
	if key == s.failKey {
		return errors.New("synthetic interrupted write")
	}
	if key == s.metadataKey && s.corruptMetadata {
		data = []byte("corrupt committed metadata")
	}
	if err := s.MemoryStore.Put(ctx, key, data); err != nil {
		return err
	}
	if key == s.metadataKey && s.uncertain {
		s.uncertain = false
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (s *publicationFaultStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	s.stats++
	return s.MemoryStore.Stat(ctx, key)
}

func sourceSetFixture() []storage.SourcePublication {
	var out []storage.SourcePublication
	for _, body := range []string{"current filtered revision", "preserved filtered revision"} {
		bytes := []byte(body)
		digest := storage.SHA256Hex(bytes)
		out = append(out, storage.SourcePublication{Key: "sessions/codex/s/source." + digest + ".jsonl.gz", SHA256: digest, Size: len(bytes), Bytes: bytes})
	}
	return out
}

func TestSourceSetInterruptionAndExactNextReplay(t *testing.T) {
	for _, boundary := range []string{"current", "preserved", "metadata", "uncertain", "completed"} {
		t.Run(boundary, func(t *testing.T) {
			sources := sourceSetFixture()
			key := "sessions/codex/s/metadata.json"
			remote := &publicationFaultStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: key}
			switch boundary {
			case "current":
				remote.failKey = sources[0].Key
			case "preserved":
				remote.failKey = sources[1].Key
			case "metadata":
				remote.failKey = key
			case "uncertain":
				remote.uncertain = true
			}
			next := []byte("exact frozen next metadata")
			prior := storage.MetadataPredecessor{Known: true}
			err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, next, prior, storage.RetryPolicy{MaxAttempts: 1})
			if boundary == "completed" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("interruption did not surface")
			}
			if boundary != "uncertain" && boundary != "completed" {
				if _, err := remote.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
					t.Fatal("metadata preceded complete source verification", err)
				}
			}
			remote.failKey = ""
			if err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, next, prior, storage.RetryPolicy{MaxAttempts: 1}); err != nil {
				t.Fatal(err)
			}
			puts := remote.puts
			stats := remote.stats
			if err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, next, storage.MetadataPredecessor{}, storage.RetryPolicy{MaxAttempts: 1}); err != nil {
				t.Fatal("exact next must finish even with unknown predecessor", err)
			}
			if remote.puts != puts || remote.stats-stats != len(sources) {
				t.Fatal("replay must verify each source exactly once without puts", remote.puts-puts, remote.stats-stats)
			}
		})
	}
}

func TestSourceSetPredecessorConflictNeverOverwritesWinner(t *testing.T) {
	for _, mode := range []string{"known-present", "unknown", "absent", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			sources := sourceSetFixture()
			key := "sessions/codex/s/metadata.json"
			remote := &publicationFaultStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: key}
			winner := []byte("different committed owner or mutation")
			if mode == "unreadable" {
				winner = []byte("{incomplete")
			}
			if err := remote.MemoryStore.Put(t.Context(), key, winner); err != nil {
				t.Fatal(err)
			}
			prior := storage.MetadataPredecessor{}
			if mode == "known-present" {
				prior = storage.MetadataPredecessor{Known: true, Exists: true, SHA256: storage.SHA256Hex([]byte("obsolete predecessor"))}
			}
			if mode == "absent" {
				prior.Known = true
			}
			err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, []byte("next"), prior, storage.RetryPolicy{MaxAttempts: 1})
			if !errors.Is(err, storage.ErrPublicationConflict) || remote.puts != 0 {
				t.Fatal("conflict mutated remote state", err, remote.puts)
			}
			got, err := remote.Get(t.Context(), key)
			if err != nil || string(got) != string(winner) {
				t.Fatal("winner changed", err)
			}
		})
	}
}

func TestSourceSetCorruptionAndRefOnlyMissingRemainUncommitted(t *testing.T) {
	for _, mode := range []string{"corrupt-existing", "missing-preserved", "metadata-readback"} {
		t.Run(mode, func(t *testing.T) {
			sources := sourceSetFixture()
			key := "sessions/codex/s/metadata.json"
			remote := &publicationFaultStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: key}
			if mode == "corrupt-existing" {
				if err := remote.MemoryStore.Put(t.Context(), sources[1].Key, []byte("wrong bytes")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-preserved" {
				sources[1].Bytes = nil
			}
			if mode == "metadata-readback" {
				remote.corruptMetadata = true
			}
			err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, []byte("next"), storage.MetadataPredecessor{Known: true}, storage.RetryPolicy{MaxAttempts: 1})
			want := storage.ErrChecksumMismatch
			if mode == "missing-preserved" {
				want = storage.ErrNotFound
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			if mode != "metadata-readback" {
				if _, err := remote.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
					t.Fatal("incomplete set became authoritative", err)
				}
			}
			if mode == "corrupt-existing" {
				got, err := remote.Get(t.Context(), sources[1].Key)
				if err != nil || string(got) != "wrong bytes" {
					t.Fatal("immutable mismatching key overwritten", err)
				}
			}
		})
	}
}

func TestSourceSetVerifiedPredecessorAndCancellation(t *testing.T) {
	remote := &publicationFaultStore{MemoryStore: storagetest.NewMemoryStore()}
	key := "sessions/codex/s/metadata.json"
	old := []byte("previous exact body")
	if err := remote.MemoryStore.Put(t.Context(), key, old); err != nil {
		t.Fatal(err)
	}
	prior := storage.MetadataPredecessor{Known: true, Exists: true, SHA256: storage.SHA256Hex(old)}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := storage.PutSourceSetThenMetadata(cancelled, remote, sourceSetFixture(), key, []byte("next"), prior, storage.RetryPolicy{}); !errors.Is(err, context.Canceled) || remote.puts != 0 {
		t.Fatal(err, remote.puts)
	}
	if err := storage.PutSourceSetThenMetadata(t.Context(), remote, sourceSetFixture(), key, []byte("next"), prior, storage.RetryPolicy{}); err != nil {
		t.Fatal(err)
	}
}
