package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

const maxCoverageRequests = 256
const maxCoverageRequestBytes = 1 << 20
const maxCoverageBytes = 16 << 20

// coverageInventory is bounded REQUESTED evidence, not a global native index.
// Enumerating stores only directory digests and requested matching facts. A
// second shared streaming pass validates ALL entry fingerprints before complete
// evidence becomes usable. Stamp comparison has the ordinary filesystem model:
// same-stamp out-of-band rewrites and changes after observation are not detected.
type coverageInventory struct {
	Version     int                          `json:"version"`
	Epoch       uint64                       `json:"epoch"`
	Phase       string                       `json:"phase"`
	Roots       []string                     `json:"roots"`
	Requests    map[string]coverageRequest   `json:"requests"`
	Directories map[string]coverageDirectory `json:"directories"`
	Validation  []directory                  `json:"validation"`
	FinalOffset int                          `json:"final_offset,omitempty"`
	proofEpoch  uint64
	Failed      bool `json:"failed,omitempty"`
}

type coverageRequest struct {
	TargetEpoch   uint64                       `json:"target_epoch"`
	CompleteEpoch uint64                       `json:"complete_epoch,omitempty"`
	Overflow      bool                         `json:"overflow,omitempty"`
	Candidates    map[string]coverageCandidate `json:"candidates"`
}

type coverageCandidate struct {
	Source   SourceDescriptor        `json:"source"`
	Stamp    Fingerprint             `json:"stamp"`
	Identity codexmeta.CodexIdentity `json:"identity"`
}
type coverageDirectory struct {
	Directory        directory `json:"directory"`
	Stamp            string    `json:"stamp"`
	Digest           string    `json:"digest"`
	Offset           int64     `json:"offset"`
	Complete         bool      `json:"complete"`
	Validated        bool      `json:"validated"`
	ValidationDigest string    `json:"validation_digest,omitempty"`
}

// coverageBatch keeps one bounded getdents batch's fingerprints. These include
// irrelevant regular files, directories and symlinks, so an unrelated header
// rewrite cannot hide behind an unchanged directory mtime during validation.
type coverageBatch struct {
	Stamp       string
	Entries     []string
	Unavailable bool
}

func entryFingerprint(name string, info os.FileInfo) string {
	identity := ""
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity = fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	}
	raw := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", name, identity, info.Mode(), info.Size(), info.ModTime().UnixNano())
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func appendCoverageDigest(prior string, entries []string) string {
	digest, err := hex.DecodeString(prior)
	if err != nil || len(digest) != sha256.Size {
		digest = make([]byte, sha256.Size)
	}
	for _, entry := range entries {
		sum := sha256.Sum256(append(digest, []byte(entry)...))
		digest = sum[:]
	}
	return hex.EncodeToString(digest)
}

func newCoverage(roots []string) *coverageInventory {
	return &coverageInventory{Version: 1, Epoch: 1, Phase: "observe", Roots: slices.Clone(roots), Requests: map[string]coverageRequest{}, Directories: map[string]coverageDirectory{}}
}

func (c *coverageInventory) validate(roots []string) error {
	if c == nil {
		return nil
	}
	if c.Version != 1 {
		return errors.New("native coverage requires a newer writer")
	}
	if c.Epoch == 0 || !slices.Equal(c.Roots, roots) || (c.Phase != "observe" && c.Phase != "validate" && c.Phase != "complete") || len(c.Requests) > maxCoverageRequests || len(c.Directories) > maxDirectories || len(c.Validation) > maxDirectories || c.FinalOffset < 0 || c.FinalOffset > len(c.Directories) {
		return errors.New("invalid native coverage checkpoint")
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > maxCoverageBytes {
		return errors.New("native coverage byte limit")
	}
	for _, request := range c.Requests {
		raw, err := json.Marshal(request)
		if err != nil || len(raw) > maxCoverageRequestBytes || len(request.Candidates) > 64 {
			return errors.New("native coverage request limit")
		}
	}
	for _, entry := range c.Directories {
		if entry.Offset < 0 || !slices.Contains(roots, entry.Directory.Root) || filepath.IsAbs(entry.Directory.Path) || filepath.Clean(entry.Directory.Path) != entry.Directory.Path {
			return errors.New("invalid native coverage directory")
		}
	}
	if err := c.validateCompleteProof(); err != nil {
		return err
	}
	if c.Phase == "complete" && !c.Failed {
		c.proofEpoch = c.Epoch
	}
	return nil
}

func (c *coverageInventory) request(id string) bool {
	if _, present := c.Requests[id]; present {
		return true
	}
	if len(c.Requests) >= maxCoverageRequests {
		return false
	}
	// A request entering mid-round waits for an entire following epoch. Earlier
	// observations cannot prove absence for something that was not requested yet.
	target := c.Epoch + 1
	if c.Phase == "observe" && len(c.Directories) == 0 {
		target = c.Epoch
	}
	c.Requests[id] = coverageRequest{TargetEpoch: target, Candidates: map[string]coverageCandidate{}}
	return true
}

func (c *coverageInventory) observe(source SourceDescriptor, stamp Fingerprint, id codexmeta.CodexIdentity) {
	for key, request := range c.Requests {
		if key != id.ThreadID && key != id.RolloutID || c.Phase != "observe" || request.TargetEpoch > c.Epoch {
			continue
		}
		if _, present := request.Candidates[source.Locator]; !present && len(request.Candidates) >= 64 {
			request.Overflow = true
			c.Requests[key] = request
			continue
		}
		request.Candidates[source.Locator] = coverageCandidate{source, stamp, id}
		raw, err := json.Marshal(request)
		if err != nil || len(raw) > maxCoverageRequestBytes {
			delete(request.Candidates, source.Locator)
			request.Overflow = true
		}
		c.Requests[key] = request
	}
}

func coverageKey(d directory) string { return filepath.Join(d.Root, d.Path) }

// recordBatch advances only when the caller consumed the whole batch. Retried
// getdents batches keep the old cookie and do not duplicate the digest chain.
func (c *coverageInventory) recordBatch(d directory, b coverageBatch, next int64, complete, advanced bool) {
	if c.Phase != "observe" || !advanced {
		return
	}
	key := coverageKey(d)
	prior, present := c.Directories[key]
	if b.Unavailable || len(b.Stamp) != 64 || !present && len(c.Directories) >= maxDirectories {
		c.Failed = true
		return
	}
	if !present {
		if d.Offset != 0 {
			c.Failed = true
			return
		}
		prior = coverageDirectory{Directory: d, Stamp: b.Stamp}
	}
	if prior.Offset != d.Offset || prior.Stamp != b.Stamp {
		c.Failed = true
		return
	}
	prior.Digest = appendCoverageDigest(prior.Digest, b.Entries)
	prior.Offset = next
	prior.Complete = complete
	c.Directories[key] = prior
}

func (c *coverageInventory) beginValidation() {
	if c.Phase != "observe" {
		return
	}
	c.Phase = "validate"
	c.Validation = nil
	for _, entry := range c.Directories {
		if !entry.Complete {
			c.Failed = true
		}
		d := entry.Directory
		d.Offset = 0
		c.Validation = append(c.Validation, d)
	}
	slices.SortFunc(c.Validation, func(a, b directory) int {
		if coverageKey(a) < coverageKey(b) {
			return -1
		}
		if coverageKey(a) > coverageKey(b) {
			return 1
		}
		return 0
	})
}

func (c *coverageInventory) validateBatch(d directory, b coverageBatch, next int64, complete bool) {
	if c.Phase != "validate" {
		return
	}
	key := coverageKey(d)
	prior, present := c.Directories[key]
	if !present || b.Unavailable || prior.Stamp != b.Stamp {
		c.Failed = true
		return
	}
	prior.ValidationDigest = appendCoverageDigest(prior.ValidationDigest, b.Entries)
	if complete {
		prior.Validated = prior.ValidationDigest == prior.Digest
		if !prior.Validated {
			c.Failed = true
		}
	}
	c.Directories[key] = prior
}

func (c *coverageInventory) finishValidation() bool {
	if c.Phase != "validate" || len(c.Validation) != 0 {
		return false
	}
	for _, entry := range c.Directories {
		if !entry.Validated {
			c.Failed = true
		}
	}
	c.Phase = "complete"
	if err := c.validateCompleteProof(); err != nil {
		c.Failed = true
	} else if !c.Failed {
		c.proofEpoch = c.Epoch
	}
	for key, request := range c.Requests {
		if request.TargetEpoch <= c.Epoch && !request.Overflow && !c.Failed {
			request.CompleteEpoch = c.Epoch
		} else {
			request.CompleteEpoch = 0
		}
		c.Requests[key] = request
	}
	return !c.Failed
}

func (c *coverageInventory) restart() {
	c.Epoch++
	c.proofEpoch = 0
	c.Phase = "observe"
	c.Failed = false
	c.Directories = map[string]coverageDirectory{}
	c.Validation = nil
	c.FinalOffset = 0
	for key, request := range c.Requests {
		request.Candidates = map[string]coverageCandidate{}
		request.CompleteEpoch = 0
		request.Overflow = false
		c.Requests[key] = request
	}
}

func directoryCoverageStamp(root, path string) string {
	info, err := os.Lstat(filepath.Join(root, path))
	if errors.Is(err, os.ErrNotExist) {
		sum := sha256.Sum256([]byte("absent"))
		return hex.EncodeToString(sum[:])
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	return entryFingerprint("", info)
}

func advanceCoverageValidation(ctx context.Context, c *catalog, h *Health, adapter SourceAdapter, deadline time.Time, o Options) {
	coverage := c.Coverage
	for coverage.Phase == "validate" && len(coverage.Validation) > 0 && h.Entries < 2048 && time.Now().Before(deadline) && !scanStopped(ctx, o) {
		d := coverage.Validation[0]
		batch, err := adapter.Enumerate(ctx, d.Root, d.Path, d.Offset)
		if err != nil || batch.coverage == nil {
			coverage.Failed = true
			coverage.Validation = coverage.Validation[1:]
			continue
		}
		h.Entries += len(batch.Entries)
		coverage.validateBatch(d, *batch.coverage, batch.Continuation, batch.Complete)
		if batch.Complete {
			coverage.Validation = coverage.Validation[1:]
		} else {
			d.Offset = batch.Continuation
			coverage.Validation[0] = d
		}
	}
	if coverage.Phase == "validate" && len(coverage.Validation) == 0 {
		finishCoverageDirectoryCheck(ctx, coverage, h, deadline, o)
	}
}

func finishCoverageDirectoryCheck(ctx context.Context, c *coverageInventory, h *Health, deadline time.Time, o Options) {
	keys := make([]string, 0, len(c.Directories))
	for key := range c.Directories {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for c.FinalOffset < len(keys) && h.Entries < 2048 && time.Now().Before(deadline) && !scanStopped(ctx, o) {
		entry := c.Directories[keys[c.FinalOffset]]
		if directoryCoverageStamp(entry.Directory.Root, entry.Directory.Path) != entry.Stamp {
			c.Failed = true
		}
		c.FinalOffset++
		h.Entries++
	}
	if c.FinalOffset == len(keys) {
		c.finishValidation()
	}
}

// validateCompleteProof cross-checks the persisted epoch structure. A phase
// flag alone is never absence authority after damaged local state is restored.
func (c *coverageInventory) validateCompleteProof() error {
	if c.Failed {
		return nil
	}
	if c.Phase != "complete" {
		return nil
	}
	if len(c.Roots) == 0 || len(c.Directories) == 0 || len(c.Validation) != 0 || c.FinalOffset != len(c.Directories) {
		return errors.New("incomplete native coverage proof")
	}
	for _, root := range c.Roots {
		for _, path := range []string{"sessions", "archived_sessions"} {
			if _, present := c.Directories[filepath.Join(root, path)]; !present {
				return errors.New("native coverage root proof missing")
			}
		}
	}
	for key, entry := range c.Directories {
		if key != coverageKey(entry.Directory) || !entry.Complete || !entry.Validated || entry.ValidationDigest != entry.Digest || len(entry.Stamp) != 64 || len(entry.Digest) != 64 {
			return errors.New("native coverage directory proof inconsistent")
		}
		for _, value := range []string{entry.Stamp, entry.Digest} {
			raw, err := hex.DecodeString(value)
			if err != nil || len(raw) != sha256.Size {
				return errors.New("native coverage digest invalid")
			}
		}
	}
	return c.validateCompleteRequests()

}

func (c *coverageInventory) validateCompleteRequests() error {
	for id, request := range c.Requests {
		if request.CompleteEpoch != 0 && (request.CompleteEpoch != c.Epoch || request.TargetEpoch > c.Epoch || request.Overflow) {
			return errors.New("native coverage request proof inconsistent")
		}
		for path, candidate := range request.Candidates {
			if path != candidate.Source.Locator || candidate.Source.Kind != "" || !slices.Contains(c.Roots, candidate.Source.Root) || !filepath.IsAbs(path) || filepath.Clean(path) != path || (!local.PathWithin(path, filepath.Join(candidate.Source.Root, "sessions")) && !local.PathWithin(path, filepath.Join(candidate.Source.Root, "archived_sessions"))) || candidate.Identity.ThreadID != id && candidate.Identity.RolloutID != id {
				return errors.New("native coverage candidate proof inconsistent")
			}
		}
	}
	return nil
}
