# Journals of interrupted setups

`setup-transaction.json` files as the code at the time of the fixtures' commit
writes them, replayed by `TestInterruptedSetupJournalsReplay`,
`TestAbandonRecoveryKeepsEverythingAndListsIt` and
`TestRecoveryBlockedTexts` (`internal/cli/scheduler_recovery_test.go`). A setup
interrupted under one release is recovered by the next, so these must keep
replaying unchanged. They are never regenerated to make a change pass: a new
journal format gets new fixtures beside these.

| File | The setup that was interrupted |
| --- | --- |
| `resetup-earlier-labels.json` | this installation's job loaded from an older executable's plist; two collectors under earlier labels (`relabeled` and `more_relabeled`), one loaded; setup from a newer executable, stopped where the new job was about to start |
| `first-setup-prototype.json` | a first setup that retires the prototype's running upload job (`legacy`), stopped at the same point |

## How they were made

`TestWriteInterruptedSetupFixtures` in
`internal/cli/scheduler_journal_fixtures_test.go` runs the real `setup --yes` on
a fake Mac (temporary folders, launchd replaced by a recording fake), panics
the fake `launchctl` at the bootstrap of the new job, as a crash would, so the
journal stays on disk, and copies it here. It does nothing unless asked:

```sh
AGENT_ARCHIVE_WRITE_JOURNAL_FIXTURES=1 go test ./internal/cli -run TestWriteInterruptedSetupFixtures
```

Two things differ from the bytes the code wrote. The temporary folders of the
fake Mac are rewritten to fixed paths (`/fixture/user-home`, `/fixture/account`,
`/fixture/project`, `/fixture/bin`, `/fixture/bin-new`), inside the base64
`Before` and `After` of each file as well as in the path fields, and a replay
rewrites them to its own folders; and the file is written as sorted, indented
JSON, so its keys are not in the order of the Go struct. Every field the code
wrote is kept. Values derived from a path rather than holding one are not
rewritten: the `project_id` in the recorded `config.json` hashes the
temporary project folder, and the `machine_id` is random, so both differ from
a fresh run's; recovery compares and restores bytes and reads neither.

A replay puts the fake Mac in one of three states a crash can leave: before
the first change (every file as found, every old job running), just before
the new job starts (every file changed, every old job stopped), or after it
started but before the journal was removed, then runs `agent-archive setup`
and checks that everything is put back. The tests read the fixtures as JSON
(`journalFile` in `scheduler_journal_fixtures_test.go`), not through the
code's own types.
