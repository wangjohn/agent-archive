package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/purge"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func runPurgeCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		fs := env.newCommandFlags("purge", stderr)
		if !fs.parseFlagsOnly(args) {
			return 2
		}
	}
	if len(args) == 0 {
		terminal.Println(stderr, "agent-archive: purge: choose plan or apply; run agent-archive purge --help")
		return 2
	}
	switch args[0] {
	case "plan":
		return runPurgePlan(args[1:], stdout, stderr, env)
	case "apply":
		return runPurgeApply(args[1:], stdin, stdout, stderr, env)
	default:
		terminal.Printf(stderr, "agent-archive: purge: unknown action %q; run agent-archive purge --help\n", args[0])
		return 2
	}
}

func runPurgePlan(args []string, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("purge plan", stderr)
	mode := fs.String("mode", "unreferenced", "unreferenced or old-filter")
	before := fs.String("before-filter", "", "for old-filter, include source versions below this number")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	store, cfg, found, err := openReadOnlyStore(env)
	if err != nil {
		return purgeError(stderr, err)
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}
	if *mode != "unreferenced" && *mode != "old-filter" {
		return fs.usageError("--mode must be unreferenced or old-filter")
	}
	if *mode == "old-filter" && *before == "" {
		return fs.usageError("--before-filter is required for old-filter")
	}
	if *mode == "unreferenced" && *before != "" {
		return fs.usageError("--before-filter requires --mode old-filter")
	}
	plan, err := purge.Inventory(context.Background(), store, cfg.DestinationID(), cfg.Storage.Bucket, cfg.Storage.Prefix, *mode, *before, env.now())
	if err != nil {
		return purgeError(stderr, err)
	}
	home, err := env.readHome()
	if err != nil {
		return purgeError(stderr, err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return purgeError(stderr, err)
	}
	name := hex.EncodeToString(id) + ".json"
	file := filepath.Join(home, "purge-plans", name)
	if err := local.Write(file, plan); err != nil {
		return purgeError(stderr, err)
	}
	terminal.Printf(stdout, "Plan: %s\nBucket: %s\nPrefix: %s\nExpires: %s\nDigest: %s\n", file, plan.Bucket, plan.Prefix, plan.ExpiresAt.Format("2006-01-02T15:04:05Z"), plan.Digest)
	terminal.Printf(stdout, "Unreferenced source deletion candidates (%d):\n", len(plan.Candidates))
	for _, c := range plan.Candidates {
		terminal.Printf(stdout, "  %s (%d bytes, filter %s)\n", c.Key, c.Size, c.FilterVersion)
	}
	terminal.Printf(stdout, "Still-current older-filter sessions (%d; never planned for deletion):\n", len(plan.CurrentOldSessions))
	for _, s := range plan.CurrentOldSessions {
		terminal.Printf(stdout, "  %s -> %s (filter %s)\n", s.MetadataKey, s.SourceKey, s.FilterVersion)
	}
	terminal.Println(stdout, "Pause every uploading Mac before apply. A versioned bucket also retains noncurrent versions and delete markers until an administrator removes them.")
	return 0
}

func runPurgeApply(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("purge apply", stderr)
	yes := fs.Bool("yes", false, "apply without an interactive confirmation")
	name, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}
	if name == "" {
		return fs.usageError("a PLAN path is required")
	}
	home, err := env.readHome()
	if err != nil {
		return purgeError(stderr, err)
	}
	dir := filepath.Join(home, "purge-plans")
	file := filepath.Clean(name)
	if filepath.Dir(file) != dir || filepath.Ext(file) != ".json" {
		return fs.usageError("PLAN must be a local file in %s", dir)
	}
	info, err := os.Lstat(file)
	if err != nil {
		return purgeError(stderr, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return purgeError(stderr, errors.New("plan must be a private regular file (0600)"))
	}
	var plan purge.Plan
	if err := local.Read(file, &plan); err != nil {
		return purgeError(stderr, err)
	}
	// Hold the same local lock as collection and resume through the entire
	// remote deletion pass. The pause check below must be made under it.
	unlock, err := lockCollector(home, "purge apply", env.now())
	if err != nil {
		return purgeError(stderr, fmt.Errorf("collector or settings change is running: %w", err))
	}
	defer unlock()
	cfg, found, err := config.Load(home)
	if err != nil {
		return purgeError(stderr, err)
	}
	if !found {
		return purgeError(stderr, errors.New(notSetUpMessage))
	}
	if err := plan.Validate(cfg.DestinationID(), env.now()); err != nil {
		return purgeError(stderr, err)
	}
	if plan.Bucket != cfg.Storage.Bucket || plan.Prefix != cfg.Storage.Prefix {
		return purgeError(stderr, errors.New("purge plan bucket or prefix differs from current setup"))
	}
	if !cfg.Paused {
		return purgeError(stderr, errors.New("pause this Mac and every other uploading Mac before purge apply"))
	}
	if !*yes {
		terminal.Printf(stdout, "Delete %d unreferenced sources from %s/%s? Type %s to continue: ", len(plan.Candidates), plan.Bucket, plan.Prefix, plan.Digest[:12])
		answer, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return purgeError(stderr, err)
		}
		if strings.TrimSpace(answer) != plan.Digest[:12] {
			terminal.Println(stderr, "Purge cancelled.")
			return 1
		}
	}
	store, err := env.openStore(cfg)
	if err != nil {
		return purgeError(stderr, err)
	}
	reportPath := file + ".report"
	report := purge.Report{PlanDigest: plan.Digest}
	for _, c := range plan.Candidates {
		report.Remaining = append(report.Remaining, c.Key)
	}
	if _, err := os.Stat(reportPath); err == nil {
		if err := local.Read(reportPath, &report); err != nil {
			return purgeError(stderr, err)
		}
		if report.PlanDigest != plan.Digest {
			return purgeError(stderr, errors.New("existing report belongs to another plan"))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return purgeError(stderr, err)
	}
	save := func(value purge.Report) error { return local.Write(reportPath, value) }
	if err := save(report); err != nil {
		return purgeError(stderr, err)
	}
	if err := purge.Apply(context.Background(), store, plan, &report, save); err != nil {
		report.Error = err.Error()
		if saveErr := save(report); saveErr != nil {
			return purgeError(stderr, fmt.Errorf("%w; report write: %v", err, saveErr))
		}
		return purgeError(stderr, fmt.Errorf("%w; deleted %d, remaining %d; resume before expiry with the same plan", err, len(report.Deleted), len(report.Remaining)))
	}
	terminal.Printf(stdout, "Deleted %d unreferenced sources. Report: %s\n", len(report.Deleted), reportPath)
	return 0
}

func purgeError(stderr io.Writer, err error) int {
	terminal.Printf(stderr, "agent-archive: purge: %v\n", err)
	return 1
}
