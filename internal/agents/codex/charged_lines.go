package codex

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// scanChargedLines reserves each raw line buffer before growing it, including
// both old and replacement storage during a copy. Parsed maps remain outside
// the native charged-data/RSS contract. A capacity refusal is retryable.
func (p *relatedSourcePass) scanChargedLines(ctx context.Context, reader io.Reader, limit int64, visit func([]byte) bool) error {
	const scratch int64 = 4096
	if !p.reserve(scratch) {
		return sourceFailure(agentapi.Unavailable, "native scanner scratch budget exhausted")
	}
	defer p.release(scratch)
	scanner := bufio.NewReaderSize(reader, int(scratch))
	var line []byte
	charge := int64(0)
	defer func() { p.release(charge) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		part, err := scanner.ReadSlice('\n')
		if int64(len(line)+len(part)) > limit {
			return agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
		}
		if len(line) == 0 && !errors.Is(err, bufio.ErrBufferFull) {
			if len(part) > 0 && visit(part) {
				return nil
			}
		} else {
			needed := int64(len(line) + len(part))
			if needed > int64(cap(line)) {
				capacity := min(limit, max(needed, 2*int64(cap(line))))
				if !p.reserve(capacity) {
					return sourceFailure(agentapi.Unavailable, "native record buffer budget exhausted")
				}
				grown := make([]byte, int(capacity))
				copy(grown, line)
				p.release(charge)
				charge = capacity
				line = grown[:len(line)]
			}
			line = append(line, part...)
			if !errors.Is(err, bufio.ErrBufferFull) {
				if visit(line) {
					return nil
				}
				line = line[:0]
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}
