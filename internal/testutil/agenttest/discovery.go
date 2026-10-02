package agenttest

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

type discoveryFiles struct{ opens int }

func (*discoveryFiles) ReadDir(p string) ([]fs.DirEntry, error) { return os.ReadDir(p) }
func (*discoveryFiles) Lstat(p string) (fs.FileInfo, error)     { return os.Lstat(p) }
func (f *discoveryFiles) Open(p string) (io.ReadCloser, error)  { f.opens++; return os.Open(p) }

// Discovery checks reference enumeration and caller cancellation against a
// provider-owned synthetic layout. Every fixture must contain at least two files.
// Reference discovery may not open raw transcripts before budget reservation.
func Discovery(t *testing.T, p agentapi.Discoverer, request agentapi.DiscoveryRequest, wantAgent string) {
	t.Helper()
	files := &discoveryFiles{}
	request.Files = files
	request.Stage = agentapi.DiscoveryReferences
	seen := 0
	report, err := p.Discover(t.Context(), request, func(c agentapi.DiscoveryCandidate) error {
		seen++
		if string(c.Session.Agent) != wantAgent || c.Source.Path == "" {
			t.Fatalf("unqualified reference: %+v", c)
		}
		return nil
	})
	if err != nil || report.Incomplete || seen < 2 || report.Enumerated != seen || files.opens != 0 {
		t.Fatalf("references: %+v seen=%d opens=%d err=%v", report, seen, files.opens, err)
	}
	stop := errors.New("consumer rejected reference")
	seen = 0
	_, err = p.Discover(t.Context(), request, func(agentapi.DiscoveryCandidate) error { seen++; return stop })
	if !errors.Is(err, stop) || seen != 1 {
		t.Fatalf("callback failure: seen=%d err=%v", seen, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	seen = 0
	_, err = p.Discover(ctx, request, func(agentapi.DiscoveryCandidate) error { seen++; return nil })
	if !errors.Is(err, context.Canceled) || seen != 0 {
		t.Fatalf("cancellation: seen=%d err=%v", seen, err)
	}
	request.MaxFiles = 1
	seen = 0
	report, err = p.Discover(t.Context(), request, func(agentapi.DiscoveryCandidate) error { seen++; return nil })
	if err != nil || seen != 1 || !report.Incomplete {
		t.Fatalf("cutoff: %+v seen=%d err=%v", report, seen, err)
	}
}
