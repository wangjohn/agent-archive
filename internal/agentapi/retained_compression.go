package agentapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"sync"
)

// CompressRetainedSource uses the canonical codec with the caller's shared
// ledger; the returned bytes are owned until release.
func CompressRetainedSource(ctx context.Context, bundle archive.SourceBundle, budget *NativeReadBudget) (archive.CompressedSource, func(), error) {
	if budget == nil {
		return archive.CompressedSource{}, nil, ErrReadBudget
	}
	if bundle.SchemaVersion != archive.SourceSchemaVersion && bundle.SchemaVersion != archive.HistorySourceSchemaVersion {
		return archive.CompressedSource{}, nil, errors.New("unsupported retained source schema")
	}
	if err := bundle.ValidateHistory(); err != nil {
		return archive.CompressedSource{}, nil, err
	}
	const compressorScratch = 1 << 20
	const sizingScratch = 32 << 10
	if !budget.Reserve(sizingScratch) {
		return archive.CompressedSource{}, nil, ErrReadBudget
	}
	largest, err := archive.SourceEncodingLineBound(ctx, bundle, budget.Available())
	budget.Release(sizingScratch)
	if err != nil {
		return archive.CompressedSource{}, nil, err
	}
	if largest > (budget.Available()-compressorScratch)/2 {
		return archive.CompressedSource{}, nil, ErrReadBudget
	}
	scratch := 2*largest + compressorScratch
	if !budget.Reserve(scratch) {
		return archive.CompressedSource{}, nil, ErrReadBudget
	}
	defer budget.Release(scratch)
	writer := &retainedCompressedWriter{ctx: ctx, budget: budget}
	var once sync.Once
	release := func() { once.Do(func() { budget.Release(writer.charged) }) }
	if err = archive.CompressSource(writer, bundle); err != nil {
		release()
		return archive.CompressedSource{}, nil, err
	}
	sum := sha256.Sum256(writer.Bytes())
	return archive.CompressedSource{Bytes: writer.Bytes(), SHA256: hex.EncodeToString(sum[:])}, release, nil
}

type retainedCompressedWriter struct {
	bytes.Buffer
	ctx     context.Context
	budget  *NativeReadBudget
	charged int64
}

func (w *retainedCompressedWriter) Write(raw []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n := int64(len(raw))
	if n > (128<<20)-int64(w.Len()) || !w.budget.Reserve(n) {
		return 0, ErrReadBudget
	}
	w.charged += n
	return w.Buffer.Write(raw)
}
