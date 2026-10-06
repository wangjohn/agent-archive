package reader

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// DecodeReferencedSourceLeased charges decompressed wire data before assembling
// records. The returned release owns that charge for the bundle's lifetime.
// It is a logical data bound: encoding/json map overhead and process RSS are
// not proportional to wire size and are not claimed to be bounded by it.
func DecodeReferencedSourceLeased(ctx context.Context, metadata archive.Metadata, data []byte, limits Limits, budget *agentapi.NativeReadBudget) (archive.SourceBundle, func(), error) {
	return decodeSourceLeased(ctx, data, limits, budget, func() (archive.SourceBundle, error) {
		return DecodeReferencedSource(ctx, metadata, data, limits)
	})
}

// DecodeRevisionSourceLeased is the same ownership contract for an alternative.
func DecodeRevisionSourceLeased(ctx context.Context, metadata archive.Metadata, revision string, data []byte, limits Limits, budget *agentapi.NativeReadBudget) (archive.SourceBundle, func(), error) {
	return decodeSourceLeased(ctx, data, limits, budget, func() (archive.SourceBundle, error) {
		return DecodeRevisionSource(ctx, metadata, revision, data, limits)
	})
}

var errDecodeBudget = errors.New("retained source exceeds shared data budget")

func decodeSourceLeased(ctx context.Context, data []byte, limits Limits, budget *agentapi.NativeReadBudget, decode func() (archive.SourceBundle, error)) (archive.SourceBundle, func(), error) {
	noop := func() {}
	if err := ctx.Err(); err != nil {
		return archive.SourceBundle{}, noop, err
	}
	// Reserve the bounded gzip/counting and scanner scratch before opening
	// the stream. Preflight reads wire bytes only, never JSON objects.
	const preflightScratch = 64 << 10
	if !budget.Reserve(preflightScratch) {
		return archive.SourceBundle{}, noop, errDecodeBudget
	}
	wire, err := sourceWireSize(ctx, data, int64(limits.uncompressed()))
	budget.Release(preflightScratch)
	if err != nil {
		return archive.SourceBundle{}, noop, err
	}
	scratch := int64(archive.MaxSourceLineBytes+1) + preflightScratch
	if !budget.Reserve(wire + scratch) {
		return archive.SourceBundle{}, noop, errDecodeBudget
	}
	bundle, err := decode()
	budget.Release(scratch)
	if err != nil {
		budget.Release(wire)
		return archive.SourceBundle{}, noop, err
	}
	released := false
	release := func() {
		if !released {
			budget.Release(wire)
			released = true
		}
	}
	return bundle, release, nil
}

func sourceWireSize(ctx context.Context, data []byte, limit int64) (int64, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	defer func() { _ = gz.Close() }()
	var scratch [32 << 10]byte
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, err := gz.Read(scratch[:])
		total += int64(n)
		if total > limit {
			return 0, archive.ErrSourceTooLarge
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return 0, err
		}
	}
}
