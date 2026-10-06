package storage

import (
	"context"
	"errors"
	"fmt"
)

// ErrPublicationConflict keeps an obsolete or unknown mutation pending without
// replacing a different authoritative body. Read/compare/Put requires the
// documented single-owner boundary; it is not a cross-machine atomic CAS.
var ErrPublicationConflict = errors.New("publication predecessor differs or is unknown; reconcile retained publication before retrying")

// SourcePublication is an exact checksum-addressed object with optional replay bytes.
type SourcePublication struct {
	Key    string
	SHA256 string
	Size   int
	Bytes  []byte
}

// MetadataPredecessor distinguishes unknown, known absent and exact known present.
type MetadataPredecessor struct {
	Known  bool
	Exists bool
	SHA256 string
}

// PutSourceSetThenMetadata verifies all selected source objects, reconciles an
// exact predecessor, writes the frozen next metadata and verifies its exact body.
// Missing ref-only sources and mismatching immutable objects are never replaced.
// At most 65 references and 128 MiB per object are checked sequentially; total
// referenced bytes may be larger. A completed uncertain Put replays idempotently.
func PutSourceSetThenMetadata(ctx context.Context, store ObjectStore, sources []SourcePublication, key string, metadata []byte, prior MetadataPredecessor, retry RetryPolicy) error {
	if len(sources) == 0 || len(sources) > 65 || key == "" || len(metadata) == 0 || len(metadata) > 32<<20 {
		return errors.New("publication source set or metadata exceeds bounds")
	}
	seen := map[string]bool{}
	for _, source := range sources {
		if source.Key == "" || source.Key == key || seen[source.Key] || source.Size <= 0 || source.Size > 128<<20 || len(source.SHA256) != 64 {
			return errors.New("invalid publication source bounds")
		}
		if len(source.Bytes) > 0 && (len(source.Bytes) != source.Size || !VerifySHA256(source.Bytes, source.SHA256)) {
			return ErrChecksumMismatch
		}
		seen[source.Key] = true
	}
	next := SHA256Hex(metadata)
	if _, err := publicationPosition(ctx, store, key, next, prior); err != nil {
		return err
	}
	if err := VerifySourceSet(ctx, store, sources, retry); err != nil {
		return err
	}
	return retry.run(ctx, func() error {
		completed, err := publicationPosition(ctx, store, key, next, prior)
		if err != nil {
			return err
		}
		if !completed {
			if err := store.Put(ctx, key, metadata); err != nil {
				return err
			}
		}
		body, err := ReadPublicationMetadata(ctx, store, key)
		if err != nil {
			return err
		}
		if len(body) != len(metadata) || SHA256Hex(body) != next {
			return fmt.Errorf("verify committed metadata: %w", ErrChecksumMismatch)
		}
		return nil
	})
}

// ReadPublicationMetadata bounds authoritative publication readback to 32 MiB.
// Providers with LimitedGetter enforce the allocation bound while reading.
func ReadPublicationMetadata(ctx context.Context, store ObjectStore, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var body []byte
	var err error
	if getter, ok := store.(LimitedGetter); ok {
		body, err = getter.GetLimited(ctx, key, 32<<20)
	} else {
		body, err = store.Get(ctx, key)
	}
	if err == nil && len(body) > 32<<20 {
		return nil, ErrObjectTooLarge
	}
	return body, err
}

func publicationPosition(ctx context.Context, store ObjectStore, key, next string, prior MetadataPredecessor) (bool, error) {
	body, err := ReadPublicationMetadata(ctx, store, key)
	if errors.Is(err, ErrNotFound) {
		if prior.Known && !prior.Exists {
			return false, nil
		}
		return false, ErrPublicationConflict
	}
	if err != nil {
		return false, err
	}
	digest := SHA256Hex(body)
	if digest == next {
		return true, nil
	}
	if prior.Known && prior.Exists && prior.SHA256 == digest {
		return false, nil
	}
	return false, ErrPublicationConflict
}

func publishExactSource(ctx context.Context, store ObjectStore, source SourcePublication) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := verifyStoredObject(ctx, store, source.Key, source.SHA256, source.Size)
	if err == nil || !errors.Is(err, ErrNotFound) || len(source.Bytes) == 0 {
		return err
	}
	if err := store.Put(ctx, source.Key, source.Bytes); err != nil {
		return err
	}
	return verifyStoredObject(ctx, store, source.Key, source.SHA256, source.Size)
}

// VerifySourceSet checks a bounded complete set sequentially. Ref-only entries
// are read-only; entries with frozen bytes may fill absent immutable objects.
// A mismatching existing object is never overwritten. Each object gets one
// bounded retry allowance, independently of the later metadata allowance.
func VerifySourceSet(ctx context.Context, store ObjectStore, sources []SourcePublication, retry RetryPolicy) error {
	if len(sources) == 0 || len(sources) > 65 {
		return errors.New("source-set reference count exceeds bounds")
	}
	seen := map[string]bool{}
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if source.Key == "" || seen[source.Key] || source.Size <= 0 || source.Size > 128<<20 || len(source.SHA256) != 64 {
			return errors.New("invalid source-set object bounds")
		}
		seen[source.Key] = true
		if len(source.Bytes) > 0 && (len(source.Bytes) != source.Size || !VerifySHA256(source.Bytes, source.SHA256)) {
			return ErrChecksumMismatch
		}
		if err := retry.run(ctx, func() error { return publishExactSource(ctx, store, source) }); err != nil {
			return fmt.Errorf("verify publication source %q: %w", source.Key, err)
		}
	}
	return nil
}
