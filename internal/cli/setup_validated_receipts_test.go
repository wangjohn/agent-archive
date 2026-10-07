package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

func TestSetupValidatedReceipts(t *testing.T) {
	t.Parallel()
	t.Run("account retry and normalized receipt", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		p := newPrompter(strings.NewReader("short\n"+strings.Repeat("AB", 16)+"\n"), &out)
		defer p.close()
		c := r2Creator{p: p, api: receiptAccountAPI{}, env: Env{LookupEnv: func(string) (string, bool) { return "", false }}}
		if err := c.chooseAccount(); err != nil || c.account != strings.Repeat("ab", 16) {
			t.Fatalf("account %q: %v", c.account, err)
		}
		assertSetupReceipt(t, out.String(), "Cloudflare account ID "+c.account, "short", strings.Repeat("AB", 16))
	})
	for _, tc := range []struct {
		name         string
		input        string
		jurisdiction string
		location     string
	}{{"location retry and normalization", "yes\nmars\nEU\nnowhere\nWNAM\n", "eu", "wnam"}, {"location defaults", "yes\n\n\n", "", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(tc.input), &out)
			defer p.close()
			c := r2Creator{p: p, account: cloudflaretest.AccountID}
			if err := c.askLocation(); err != nil || c.bucket.Jurisdiction != tc.jurisdiction || c.bucket.LocationHint != tc.location {
				t.Fatalf("location %v: %v", c.bucket, err)
			}
			assertSetupReceipt(t, out.String(), "Jurisdiction "+firstNonEmpty(tc.jurisdiction, "none"), "mars", "EU")
			assertSetupReceipt(t, out.String(), "Location hint "+firstNonEmpty(tc.location, "automatic"), "nowhere", "WNAM")
		})
	}
	t.Run("bucket retry normalized default", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		p := newPrompter(strings.NewReader("bad/name\n\n"), &out)
		defer p.close()
		c := r2Creator{p: p}
		name, err := c.askBucketName("Name", "ARCHIVE-BUCKET")
		if err != nil || name != "archive-bucket" {
			t.Fatalf("bucket %q: %v", name, err)
		}
		assertSetupReceipt(t, out.String(), "Bucket archive-bucket", "bad/name", "ARCHIVE-BUCKET")
	})
	for _, tc := range []struct {
		name    string
		input   string
		want    string
		wantEOF bool
	}{{"prefix retry", "prefix\n../bad\narchive/\n", "archive/", false}, {"prefix default", "prefix\n\n", "saved/", false}, {"prefix invalid then EOF", "prefix\n../bad\n", "saved/", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(tc.input), &out)
			defer p.close()
			draft := setupDraft{Config: config.Config{Storage: credentials.Config{Prefix: "saved/"}}}
			err := editSetupReview(nil, p, &draft, t.TempDir(), nil, nil)
			if errors.Is(err, io.EOF) != tc.wantEOF || (!tc.wantEOF && err != nil) || draft.Config.Storage.Prefix != tc.want {
				t.Fatalf("prefix %q: %v", draft.Config.Storage.Prefix, err)
			}
			if tc.wantEOF {
				assertNoRejectedReceipt(t, out.String(), "../bad", "Folder inside the bucket saved/")
			} else {
				assertSetupReceipt(t, out.String(), "Folder inside the bucket "+tc.want, "../bad")
			}
		})
	}
	t.Run("directory retry resolved path", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		var out bytes.Buffer
		p := newPrompter(strings.NewReader("exclude\n\n"+filepath.Join(home, "missing")+"\n~\ndone\n"), &out)
		defer p.close()
		cfg := config.Config{Harnesses: []string{"codex"}}
		if err := promptCodexExceptions(p, &cfg, home); err != nil || len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != resolvedDirectory(t, home) || cfg.Archive.Projects[0].Included {
			t.Fatalf("rules %v: %v", cfg.Archive.Projects, err)
		}
		assertSetupReceipt(t, out.String(), "Directory "+resolvedDirectory(t, home), "missing", "Directory ~")
	})
	for _, tc := range []struct {
		name    string
		input   string
		want    secureChoice
		deleted bool
	}{{"mismatched deletion", "delete\nwrong\nstop\n", secureStop, false}, {"blank keeps bucket", "delete\n\nstop\n", secureStop, false}, {"exact deletion", "delete\narchive-bucket\n", secureDelete, true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(tc.input), &out)
			defer p.close()
			creator := &receiptBucketCreator{}
			choice, err := askSecureChoice(p, creator, "archive-bucket", "fake")
			if err != nil || choice != tc.want || creator.deleted != tc.deleted {
				t.Fatalf("choice %q, deleted %v: %v", choice, creator.deleted, err)
			}
			if tc.deleted {
				assertSetupReceipt(t, out.String(), "Deletion confirmed for bucket archive-bucket", "wrong")
			} else {
				assertSetupReceipt(t, out.String(), "Deletion not confirmed; keeping bucket archive-bucket", "wrong", "Deletion confirmed")
			}
		})
	}

	t.Run("location invalid then EOF", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		p := newPrompter(strings.NewReader("yes\nmars\n"), &out)
		defer p.close()
		c := r2Creator{p: p}
		if err := c.askLocation(); !errors.Is(err, io.EOF) {
			t.Fatalf("expected EOF: %v", err)
		}
		assertNoRejectedReceipt(t, out.String(), "mars", "Jurisdiction none")
	})
}

func assertSetupReceipt(t *testing.T, output, accepted string, rejected ...string) {
	t.Helper()
	if !strings.Contains(output, "OK "+accepted) && !strings.Contains(output, symbolOK+" "+accepted) {
		t.Fatalf("missing accepted receipt %q:\n%s", accepted, output)
	}
	assertNoRejectedReceipt(t, output, rejected...)
}

func assertNoRejectedReceipt(t *testing.T, output string, rejected ...string) {
	t.Helper()
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, symbolOK) || strings.HasPrefix(line, "OK ") {
			for _, value := range rejected {
				if strings.Contains(line, value) {
					t.Fatalf("rejected input in success receipt %q:\n%s", line, output)
				}
			}
		}
	}
}

type receiptAccountAPI struct{ cloudflare.API }

func (receiptAccountAPI) Accounts(context.Context) ([]cloudflare.Account, error) { return nil, nil }

func resolvedDirectory(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

type receiptBucketCreator struct {
	BucketCreator
	deleted bool
}

func (c *receiptBucketCreator) DeleteBucket(context.Context, string) error {
	c.deleted = true
	return nil
}
