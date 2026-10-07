package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

type publishedRelease string

const (
	publishedRelease010 publishedRelease = "v0.1.0"
	publishedRelease011 publishedRelease = "v0.1.1"
)

// This opt-in acceptance check executes the actual published v0.1.1 writer,
// not a source build or a decoder model. Extended macOS supplies its asset.
func publishedWriterBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("AGENT_ARCHIVE_OLD_BINARY")
	if binary == "" {
		t.Skip("requires the checksum-pinned published Darwin v0.1.1 binary")
	}
	if runtime.GOOS != "darwin" || !filepath.IsAbs(binary) {
		t.Fatal("published acceptance requires a native Darwin absolute executable")
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	release := publishedRelease(os.Getenv("AGENT_ARCHIVE_OLD_RELEASE"))
	if release == "" {
		release = publishedRelease011
	}
	want := ""
	switch runtime.GOARCH {
	case "amd64":
		if release == publishedRelease010 {
			want = "43fbe6d8d65d2d32ebad66d55d116e10d517c40908032297bd5977beb45ab5ad"
			break
		}
		want = "c8fff68b623a7e0143503adce2d83c6494d7efa55fa0724eb4f76ae54c66255e"
	case "arm64":
		want = "09f08430627cc0c867afd41ce7eb2c3e61859b640dfb2994323577847eee06e6"
	default:
		t.Fatal("unsupported published Darwin architecture")
	}
	switch release {
	case publishedRelease010, publishedRelease011:
	default:
		t.Fatal("unsupported release")
	}
	if release == publishedRelease010 && runtime.GOARCH != "amd64" {
		t.Fatal("v0.1.0 fixture is pinned only on amd64")
	}
	if hex.EncodeToString(digest[:]) != want {
		t.Fatalf("published binary checksum differs from pinned %s asset", release)
	}
	return binary
}

func publishedPauseFixture(t *testing.T, binary string) (string, func(string) ([]byte, error)) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	userHome, temp, bin := filepath.Join(root, "home"), filepath.Join(root, "tmp"), filepath.Join(root, "bin")
	for _, dir := range []string{userHome, temp, bin} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Any unexpected scheduler or Keychain invocation fails and leaves evidence.
	marker := filepath.Join(userHome, "unexpected-service")
	for _, name := range []string{"launchctl", "systemctl", "security"} {
		stub := "#!/bin/sh\n: > \"$HOME/unexpected-service\"\nexit 99\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	pause := func(home string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "pause")
		cmd.Dir = userHome
		cmd.Env = []string{"HOME=" + userHome, "AGENT_ARCHIVE_HOME=" + home, "TMPDIR=" + temp, "PATH=" + bin, "NO_COLOR=1", "AWS_EC2_METADATA_DISABLED=true"}
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("published pause exceeded deadline: %v", ctx.Err())
		}
		if _, e := os.Stat(marker); !os.IsNotExist(e) {
			t.Fatal("published pause invoked a scheduler or Keychain command")
		}
		return out, err
	}
	return root, pause
}

func TestPublishedWriterRefusesProtectedConfiguration(t *testing.T) {
	t.Parallel()
	root, pause := publishedPauseFixture(t, publishedWriterBinary(t))
	control := filepath.Join(root, "control")
	if err := os.Mkdir(control, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{MachineID: "synthetic-old-writer", Archive: archive.Config{Enabled: true}}
	if err := Save(control, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(control, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := pause(control); err != nil {
		t.Fatalf("published scalar-schema control failed: %v %s", err, out)
	}
	after, err := os.ReadFile(filepath.Join(control, "config.json"))
	if err != nil || bytes.Equal(before, after) {
		t.Fatal("published control did not rewrite the intended archive root", err)
	}
	var paused struct {
		Paused bool `json:"paused"`
	}
	if err := json.Unmarshal(after, &paused); err != nil || !paused.Paused {
		t.Fatal("published control did not persist paused=true", err)
	}
	for _, enabled := range []bool{false, true} {
		home := filepath.Join(root, "disabled")
		if enabled {
			home = filepath.Join(root, "enabled")
		}
		if err := os.Mkdir(home, 0700); err != nil {
			t.Fatal(err)
		}
		cfg.Discovery = &DiscoveryConfig{Enabled: enabled}
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := pause(home)
		if err == nil || !strings.Contains(string(out), "cannot unmarshal object") || !strings.Contains(string(out), "schema_version") || !strings.Contains(string(out), "type int") || !strings.Contains(string(out), filepath.Join(home, "config.json")) {
			t.Fatalf("published writer did not reject protected schema in intended root (enabled=%t): %v %s", enabled, err, out)
		}
		after, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("published writer changed protected config (enabled=%t): %v", enabled, err)
		}
	}
}

func TestPublishedWriterRefusesHistoryAndStagedConfiguration(t *testing.T) {
	root, pause := publishedPauseFixture(t, publishedWriterBinary(t))
	for _, history := range []bool{false, true} {
		home := filepath.Join(root, "staged")
		if history {
			home = filepath.Join(root, "history")
		}
		if err := os.Mkdir(home, 0700); err != nil {
			t.Fatal(err)
		}
		cfg := Config{MachineID: "synthetic", Archive: archive.Config{Enabled: true}, DurableImportProtection: true, HistoryProtection: history, GenerationProtection: true}
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := pause(home)
		if err == nil || !strings.Contains(string(out), "cannot unmarshal object") {
			t.Fatal("actual protected writer accepted", string(out), err)
		}
		after, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("actual old writer changed protected config", err)
		}
	}
}
