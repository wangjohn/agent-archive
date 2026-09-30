package scheduler

import (
	"strings"
	"testing"
)

// Version 1 of the port defines files: an artifact's ID is "file:" and its
// path, and an artifact of another kind names no file.
func TestArtifactsAreFiles(t *testing.T) {
	t.Parallel()
	artifact := FileArtifact("/home/me/job", []byte("x"), 0o600)
	if artifact.ID != "file:/home/me/job" || artifact.Mode != 0o600 || string(artifact.After) != "x" {
		t.Errorf("FileArtifact = %+v", artifact)
	}
	if path, ok := artifact.Path(); !ok || path != "/home/me/job" {
		t.Errorf("Path = %q, %v", path, ok)
	}
	if path, ok := (Artifact{ID: "crontab:me"}).Path(); ok || path != "crontab:me" {
		t.Errorf("an artifact that is not a file has the path %q, %v", path, ok)
	}
}

// The typed errors of a refused Unload say which job was left alone, in the
// scheduler's own nouns, and are errors.
func TestUnloadRefusalsSayWhichJobWasLeftAlone(t *testing.T) {
	t.Parallel()
	words := Words{Manager: "the manager", Job: "job", Definition: "definition", Tool: "tool"}
	problem := Problem{Kind: ProblemNotOwned, Ref: "ref-1", Expected: "/site/ref-1.def"}
	var notOwned error = &NotOwnedError{Words: words, Problem: problem}
	if got, want := notOwned.Error(), "the manager's ref-1 job was not loaded from /site/ref-1.def; it belongs to another installation and was left running"; got != want {
		t.Errorf("NotOwnedError: %q, want %q", got, want)
	}
	var indeterminate error = &IndeterminateError{Words: words, Problem: problem}
	if got, want := indeterminate.Error(), "cannot confirm which definition the manager's ref-1 job was loaded from; it was left as it is"; got != want {
		t.Errorf("IndeterminateError: %q, want %q", got, want)
	}
	if !strings.Contains(notOwned.Error(), string(problem.Ref)) {
		t.Error("the refusal does not name the job")
	}
}
