package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// ErrQuarantined marks a local state file that no longer decoded and was
// moved aside, renamed with quarantineSuffix, so it stops failing every pass
// (and every hook that would rewrite it). The file is kept, not deleted, for
// someone to inspect; Status.QuarantinedFiles lists it until then.
var ErrQuarantined = errors.New("unreadable local state file was moved aside")

// quarantineSuffix ends a quarantined file's name, after the time it was
// moved aside (see quarantinePath). Listings only read *.json, so a
// quarantined file is never read again.
const quarantineSuffix = ".corrupt"

// quarantineDirs are the directories whose files the collector lists on
// every pass and so may quarantine.
var quarantineDirs = []string{"registrations", "requests", "subagent-candidates"}

// quarantinePath names the file path is moved aside to. The time in the name
// keeps a second quarantine of the same file (a hook rewrote it, and it was
// corrupted again) from replacing the first.
func quarantinePath(path string) string {
	return path + "." + time.Now().UTC().Format("20060102T150405.000000000Z") + quarantineSuffix
}

// isCorruptJSON reports whether err means the file's bytes are not JSON at
// all: truncated, empty, or garbled, which no version of this program could
// read either. A well-formed document of an unexpected shape
// (json.UnmarshalTypeError) is not corruption: it is likely a file a newer
// version wrote before a downgrade, and it is reported, never moved aside.
func isCorruptJSON(err error) bool {
	var syntaxErr *json.SyntaxError
	return errors.As(err, &syntaxErr)
}

// isUndecodable reports whether err is any JSON decoding failure, corrupt or
// of an unexpected shape.
func isUndecodable(err error) bool {
	var typeErr *json.UnmarshalTypeError
	return isCorruptJSON(err) || errors.As(err, &typeErr)
}

// readOrQuarantine reads the JSON file at path. found is false when it does
// not exist. A file that does not decode is read again under lockName, the
// lock its writers hold, so a writer replacing it at that moment is not
// mistaken for corruption; if it still does not decode it is renamed aside
// and the error wraps ErrQuarantined. Any other read failure is returned as
// is and leaves the file where it is, to be retried.
func readOrQuarantine[T any](s *LocalStore, path, lockName string) (value T, found bool, err error) {
	value, found, err = readJSON[T](path)
	if err == nil || !isCorruptJSON(err) {
		return value, found, err
	}
	unlock, lockErr := local.NamedLockWait(s.home, lockName, time.Second)
	if lockErr != nil {
		return value, false, fmt.Errorf("%w (and it could not be locked to move it aside: %w)", err, lockErr)
	}
	defer unlock()
	value, found, err = readJSON[T](path)
	if err == nil || !isCorruptJSON(err) {
		return value, found, err
	}
	rel, relErr := filepath.Rel(s.home, path)
	if relErr != nil {
		rel = path
	}
	aside := quarantinePath(path)
	if renameErr := os.Rename(path, aside); renameErr != nil {
		return value, false, fmt.Errorf("%w (and it could not be moved aside: %w)", err, renameErr)
	}
	pruneQuarantine(path)
	return value, false, fmt.Errorf("%w: %s did not decode (%w) and is now %s", ErrQuarantined, rel, err, filepath.Join(filepath.Dir(rel), filepath.Base(aside)))
}

// quarantineKeep is how many quarantined copies of one file are kept. A file
// a hook keeps rewriting and something keeps corrupting would otherwise pile
// up a copy per pass.
const quarantineKeep = 3

// pruneQuarantine removes all but the newest quarantineKeep quarantined
// copies of path. Their names sort by the time they were moved aside. Best
// effort: a copy left behind is only clutter.
func pruneQuarantine(path string) {
	copies, err := filepath.Glob(path + ".*" + quarantineSuffix)
	if err != nil || len(copies) <= quarantineKeep {
		return
	}
	sort.Strings(copies)
	for _, old := range copies[:len(copies)-quarantineKeep] {
		_ = os.Remove(old)
	}
}

func readJSON[T any](path string) (T, bool, error) {
	var value T
	err := local.Read(path, &value)
	if errors.Is(err, os.ErrNotExist) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	return value, true, nil
}

// listJSONStems returns the names, without ".json", of the JSON files in one
// of the store's directories.
func (s *LocalStore) listJSONStems(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.home, dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stems []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		stem := strings.TrimSuffix(entry.Name(), ".json")
		if safeFileComponent(stem) {
			stems = append(stems, stem)
		}
	}
	return stems, nil
}

// ScanRegistrations is LoadRegistrations for the collector pass and the
// retention sweep, which run under the collector lock: one registration that
// cannot be read fails only its own session. It is left out of regs and
// reported in issues under its archive session ID, and one that does not
// decode is quarantined (see ErrQuarantined), so a hook for the same native
// session can register it again. err is reserved for a directory that cannot
// be listed at all.
func (s *LocalStore) ScanRegistrations() (regs []archive.SessionRegistration, issues map[string]error, err error) {
	ids, err := s.listJSONStems("registrations")
	if err != nil {
		return nil, nil, fmt.Errorf("list registrations: %w", err)
	}
	issues = map[string]error{}
	regs = make([]archive.SessionRegistration, 0, len(ids))
	for _, id := range ids {
		reg, found, err := readOrQuarantine[archive.SessionRegistration](s, s.registrationPath(id), requestLockName(id))
		if err != nil {
			issues[id] = fmt.Errorf("read registration %q: %w", id, err)
			continue
		}
		if found {
			regs = append(regs, reg)
		}
	}
	sort.Slice(regs, func(i, j int) bool { return regs[i].ArchiveSessionID < regs[j].ArchiveSessionID })
	return regs, issues, nil
}

// ScanRequests is LoadRequests with ScanRegistrations' isolation: a request
// that cannot be read is reported in issues under its archive session ID,
// and one that does not decode is quarantined. Its hook evidence is lost
// either way, but hooks can queue the session's requests again, which they
// cannot while it sits there.
func (s *LocalStore) ScanRequests() (requests []Request, issues map[string]error, err error) {
	ids, err := s.listJSONStems("requests")
	if err != nil {
		return nil, nil, fmt.Errorf("list requests: %w", err)
	}
	issues = map[string]error{}
	requests = make([]Request, 0, len(ids))
	for _, id := range ids {
		req, found, err := readOrQuarantine[Request](s, s.requestPath(id), requestLockName(id))
		if err != nil {
			issues[id] = fmt.Errorf("read request %q: %w", id, err)
			continue
		}
		if found {
			requests = append(requests, req)
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ArchiveSessionID < requests[j].ArchiveSessionID })
	return requests, issues, nil
}

// scanSubagentCandidates is LoadSubagentCandidates with ScanRegistrations'
// isolation, keyed by the candidate's archive session ID.
func (s *LocalStore) scanSubagentCandidates() ([]SubagentCandidate, map[string]error, error) {
	ids, err := s.listJSONStems("subagent-candidates")
	if err != nil {
		return nil, nil, fmt.Errorf("list subagent candidates: %w", err)
	}
	issues := map[string]error{}
	var out []SubagentCandidate
	for _, id := range ids {
		candidate, found, err := readOrQuarantine[SubagentCandidate](s, s.subagentCandidatePath(id), subagentLockName(id))
		if err != nil {
			issues[id] = fmt.Errorf("read subagent candidate %q: %w", id, err)
			continue
		}
		if found {
			out = append(out, candidate)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, issues, nil
}

// quarantinedFiles lists the files ever quarantined that are still present,
// relative to the archive directory.
func (s *LocalStore) quarantinedFiles() []string {
	var out []string
	for _, dir := range quarantineDirs {
		matches, _ := filepath.Glob(filepath.Join(s.home, dir, "*"+quarantineSuffix))
		for _, match := range matches {
			if rel, err := filepath.Rel(s.home, match); err == nil {
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	return out
}

// staleTempAge is how old an atomic-write temporary file (see
// local.RemoveStaleTemps) must be before a pass removes it. A write takes
// well under a second, so anything this old was left by a process that
// died mid-write.
const staleTempAge = time.Hour

// removeStaleTemps removes atomic-write temporaries that crashed writers
// left in the directories this store owns. Best effort: a failure only
// leaves the file for the next pass.
func (s *LocalStore) removeStaleTemps() {
	dirs := []string{s.home}
	for _, dir := range append(append([]string{}, storeDirs...), lazyStoreDirs...) {
		dirs = append(dirs, filepath.Join(s.home, dir))
	}
	// Per-session evidence directories (see SessionDir) sit one level down.
	if entries, err := os.ReadDir(filepath.Join(s.home, "sessions")); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(s.home, "sessions", entry.Name()))
			}
		}
	}
	for _, dir := range dirs {
		_ = local.RemoveStaleTemps(dir, staleTempAge)
	}
}
