// Package evidence produces bounded, privacy-filtered observations which are
// kept separate from an application's native transcript.
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
)

const (
	maxSkillsPerRoot = 256
	maxSkillBytes    = 1 << 20
	maxSnapshotBytes = 4 << 20
	// maxSnapshotBodyBytes caps one archived SKILL.md body. Every session's
	// evidence embeds the bodies of the skills installed when it ran, so an
	// uncapped body is repeated in every bundle. The inventory entry (name,
	// sha256, scope) and the recorded original size stay complete, and the
	// sha256 remains the hash of the whole original file. Moving bodies to
	// content-addressed objects is deferred.
	maxSnapshotBodyBytes = 16 << 10
)

// SkillOptions identifies the only filesystem locations a background
// collection pass may inspect. ProjectRoot is already an explicitly included
// project and UserHome is the local user's home; no repository-wide walk is
// performed.
type SkillOptions struct {
	Harness     string
	ProjectRoot string
	UserHome    string
	ObservedAt  time.Time
}

type skillRoot struct {
	path  string
	scope string
	// project is the project root a project-level skill root belongs to, and
	// empty for a user-level root. It bounds where a project skill's SKILL.md
	// may resolve to (see skillBounds).
	project string
}

// ObserveSkills inventories immediate SKILL.md children of documented skill
// roots. Filesystem presence proves installation only. It deliberately does
// not claim that the harness discovered, exposed, or invoked a skill in this
// session. The returned evidence has already passed the archive privacy
// filter and is safe to place in a local upload request or source candidate.
//
// A skills directory the collector cannot read is a coverage gap for that
// root, never a reason to withhold the session: an unreadable root yields an
// "unreadable" inventory and an unreadable entry counts as uninspected.
func ObserveSkills(options SkillOptions) ([]archive.SupplementalEvidence, error) {
	if options.ObservedAt.IsZero() {
		return nil, errors.New("skill observation time is required")
	}
	roots := skillRoots(options)
	remainingSnapshotBytes := int64(maxSnapshotBytes)
	var observations []archive.SupplementalEvidence
	for _, root := range roots {
		evidence, err := observeRoot(options.Harness, root, options.ObservedAt, &remainingSnapshotBytes)
		if err != nil {
			return nil, err
		}
		observations = append(observations, evidence...)
	}
	filtered, _, err := archive.FilterSupplementalEvidence(observations)
	if err != nil {
		return nil, fmt.Errorf("filter skill evidence: %w", err)
	}
	return filtered, nil
}

func skillRoots(options SkillOptions) []skillRoot {
	project, user := filepath.Clean(options.ProjectRoot), filepath.Clean(options.UserHome)
	if options.ProjectRoot == "" {
		project = ""
	}
	if options.UserHome == "" {
		user = ""
	}
	var roots []skillRoot
	addUser := func(suffix, scope string) {
		if user != "" {
			roots = append(roots, skillRoot{path: filepath.Join(user, suffix), scope: scope})
		}
	}
	addProject := func(suffix, scope string) {
		if project != "" {
			roots = append(roots, skillRoot{path: filepath.Join(project, suffix), scope: scope, project: project})
		}
	}
	switch strings.ToLower(strings.TrimSpace(options.Harness)) {
	case "codex":
		addUser(".agents/skills", "user_agents")
		addUser(".codex/skills", "user_codex_legacy")
		addProject(".agents/skills", "project_agents")
	case "claude", "claude-code":
		addUser(".claude/skills", "user_claude")
		addProject(".claude/skills", "project_claude")
	case "cursor":
		addUser(".cursor/skills", "user_cursor")
		addProject(".cursor/skills", "project_cursor")
	}
	return roots
}

func observeRoot(harness string, root skillRoot, observedAt time.Time, remainingSnapshotBytes *int64) ([]archive.SupplementalEvidence, error) {
	entries, err := os.ReadDir(root.path)
	if errors.Is(err, os.ErrNotExist) {
		// Root-local absence is observable and lets a later pass record removal
		// of every skill previously seen in this scope. Installed-only coverage
		// still says nothing about what the harness discovered or invoked.
		return []archive.SupplementalEvidence{inventoryObservation(harness, root.scope, "absent", nil, true, observedAt)}, nil
	}
	if err != nil {
		// Not a directory, permission denied, or any other read failure: the
		// root exists but its inventory is unknown. Record that instead of
		// failing publication of a session over an unrelated directory.
		return []archive.SupplementalEvidence{inventoryObservation(harness, root.scope, "unreadable", nil, false, observedAt)}, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	omittedEntries := 0
	if len(entries) > maxSkillsPerRoot {
		omittedEntries = len(entries) - maxSkillsPerRoot
		entries = entries[:maxSkillsPerRoot]
	}
	provenance := "filesystem:" + strings.ToLower(strings.TrimSpace(harness))
	var inventory []any
	var snapshots []archive.SupplementalEvidence
	omittedSnapshots := 0
	uninspectedEntries := 0
	bounds := newSkillBounds(root)
	for _, entry := range entries {
		// Symlinks are resolved first, and the SKILL.md actually read is the
		// resolved file, which must be a regular file inside the root's bounds.
		path, err := filepath.EvalSymlinks(filepath.Join(root.path, entry.Name(), "SKILL.md"))
		if errors.Is(err, os.ErrNotExist) {
			// Some legacy/configured roots contain grouped or plugin-managed
			// subtrees. We do not recursively walk them; record the coverage gap.
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				uninspectedEntries++
			}
			continue
		}
		if err != nil || !bounds.allow(path) {
			// Permission denied on the entry or its SKILL.md, or a SKILL.md that
			// resolves outside the root's bounds: coverage gap.
			uninspectedEntries++
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			uninspectedEntries++
			continue
		}
		name := entry.Name()
		payload := map[string]any{
			"name": name, "coverage": string(archive.SkillCoverageInstalledOnly), "scope": root.scope,
			"uncertainty": "filesystem presence does not prove discovery, eligibility, or invocation",
		}
		if info.Size() > maxSkillBytes {
			inventory = append(inventory, map[string]any{"name": name})
			omittedSnapshots++
			continue
		}
		if info.Size() > *remainingSnapshotBytes {
			inventory = append(inventory, map[string]any{"name": name})
			omittedSnapshots++
			continue
		}
		original, err := readBounded(path, maxSkillBytes)
		if err != nil {
			uninspectedEntries++
			continue
		}
		*remainingSnapshotBytes -= int64(len(original))
		if parsed := frontmatterName(original); parsed != "" {
			name = parsed
			payload["name"] = name
		}
		digest := sha256.Sum256(original)
		hash := hex.EncodeToString(digest[:])
		payload["sha256"] = hash // hash of original bytes, before filtering
		body := string(original)
		if len(body) > maxSnapshotBodyBytes {
			body = truncateUTF8(body, maxSnapshotBodyBytes)
			payload["truncated"] = true
		}
		payload["snapshot"] = body
		payload["original_bytes"] = float64(len(original))
		payload["redacted"] = false
		filtered, gaps, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
			Kind: archive.EvidenceKindSkillSnapshot, ObservedAt: observedAt, Provenance: provenance, Payload: payload,
		}})
		if err != nil {
			return nil, err
		}
		if len(filtered) == 0 {
			continue
		}
		archive.AnnotateSupplementalGaps(filtered[0].Payload, gaps)
		inventory = append(inventory, map[string]any{"name": name, "sha256": hash})
		snapshots = append(snapshots, filtered[0])
	}
	totalOmittedEntries := omittedEntries + uninspectedEntries
	rootStatus := "present"
	if len(inventory) == 0 && totalOmittedEntries == 0 {
		rootStatus = "empty"
	}
	inventoryPayload := map[string]any{
		"coverage": string(archive.SkillCoverageInstalledOnly), "scope": root.scope, "skills": inventory,
		"inventory_complete": totalOmittedEntries == 0,
		"root_status":        rootStatus,
		"uncertainty":        "root-local filesystem inventory does not prove discovery, eligibility, or invocation; other roots and plugin-managed inventories may not have been inspected",
	}
	if totalOmittedEntries > 0 {
		inventoryPayload["truncated"] = true
		inventoryPayload["omitted_count"] = float64(totalOmittedEntries)
	}
	if omittedSnapshots > 0 {
		inventoryPayload["snapshot_omitted_count"] = float64(omittedSnapshots)
	}
	result := []archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindSkillInventory, ObservedAt: observedAt, Provenance: provenance,
		Payload: inventoryPayload,
	}}
	return append(result, snapshots...), nil
}

// skillBounds decides which resolved SKILL.md paths one skill root may read.
// Without it a SKILL.md, or the skill directory holding it, could be a
// symlink to any file the collector can read, and a repository a person
// merely cloned could ship .claude/skills/x/SKILL.md -> ~/.aws/credentials
// to have that file archived with every session as a skill snapshot.
//
//   - A project-level root belongs to a repository, which is not trusted to
//     name files outside itself. Its SKILL.md must resolve to a file inside
//     the project root (symlinks resolved on both sides). Links within the
//     repository keep working, such as .claude/skills/x -> ../../skills/x,
//     which lets one skills directory serve several harnesses.
//   - A user-level root is the person's own configuration, where linking a
//     skill directory into a skills checkout elsewhere (~/.claude/skills/x ->
//     ~/src/skills/x) is a supported way to install one. Its SKILL.md may
//     resolve anywhere inside the root itself, or anywhere at all as long as
//     the resolved file is itself named SKILL.md: a linked skill, not an
//     arbitrary file under a skill's name.
//
// An entry that resolves outside its bounds counts as uninspected. The
// bounds are checked on the resolved path, and the resolved path is what is
// read; a symlink swapped in between the two is a race this does not close.
type skillBounds struct {
	// within is the resolved directory a SKILL.md may resolve inside, or ""
	// when it could not be resolved.
	within string
	// linkedSkill allows a SKILL.md resolving anywhere if it is named SKILL.md.
	linkedSkill bool
}

func newSkillBounds(root skillRoot) skillBounds {
	base := root.path
	if root.project != "" {
		base = root.project
	}
	bounds := skillBounds{linkedSkill: root.project == ""}
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		bounds.within = resolved
	}
	return bounds
}

// allow reports whether a resolved SKILL.md path is inside the bounds.
func (b skillBounds) allow(resolved string) bool {
	if b.linkedSkill && strings.EqualFold(filepath.Base(resolved), "SKILL.md") {
		return true
	}
	if b.within == "" {
		return false
	}
	rel, err := filepath.Rel(b.within, resolved)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// truncateUTF8 returns at most n bytes of s without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func inventoryObservation(harness, scope, rootStatus string, skills []any, complete bool, observedAt time.Time) archive.SupplementalEvidence {
	if skills == nil {
		skills = []any{}
	}
	return archive.SupplementalEvidence{
		Kind: archive.EvidenceKindSkillInventory, ObservedAt: observedAt,
		Provenance: "filesystem:" + strings.ToLower(strings.TrimSpace(harness)),
		Payload: map[string]any{
			"coverage":           string(archive.SkillCoverageInstalledOnly),
			"scope":              scope,
			"skills":             skills,
			"inventory_complete": complete,
			"root_status":        rootStatus,
			"uncertainty":        "root-local filesystem inventory does not prove discovery, eligibility, or invocation; other roots and plugin-managed inventories may not have been inspected",
		},
	}
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("source exceeded size limit while reading")
	}
	return data, nil
}

func frontmatterName(content []byte) string {
	lines := strings.Split(string(content), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		if !strings.HasPrefix(strings.ToLower(trimmed), "name:") {
			continue
		}
		name := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, trimmed[:5])), "\"'")
		if name != "" && len(name) <= 256 && !strings.ContainsAny(name, "\r\n\x00") {
			return name
		}
	}
	return ""
}
