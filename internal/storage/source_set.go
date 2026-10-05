package storage

import (
	"context"
	"errors"
	"fmt"
)

// PutVerifiedSource stores immutable bytes only when absent, then verifies them.
// It never changes a metadata pointer and refuses an existing different object.
func PutVerifiedSource(ctx context.Context, store ObjectStore, key, sha string, data []byte, retry RetryPolicy) error {
	if key == "" || !VerifySHA256(data, sha) {
		return errors.New("invalid immutable source checksum or key")
	}
	return retry.run(ctx, func() error {
		err := verifyStoredObject(ctx, store, key, sha, len(data))
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("verify immutable source: %w", err)
		}
		if err := store.Put(ctx, key, data); err != nil {
			return err
		}
		return verifyStoredObject(ctx, store, key, sha, len(data))
	})
}

// VerifySource verifies an already referenced object before metadata publication.
func VerifySource(ctx context.Context, store ObjectStore, key, sha string, size int, retry RetryPolicy) error {
	if key == "" || sha == "" || size <= 0 {
		return errors.New("invalid source reference")
	}
	return retry.run(ctx, func() error { return verifyStoredObject(ctx, store, key, sha, size) })
}
