# Local session identity and recovery

Local registrations use the agent and its exact native session ID together.
Claude Code, Codex and Cursor can use the same native ID without sharing an
archive session. Agent aliases are canonicalized; native IDs retain their exact
valid UTF-8 bytes, including case and whitespace. Empty/all-whitespace IDs and
invalid UTF-8 identities are refused. Archive session IDs, parent archive IDs,
remote object keys, source/metadata schemas and pending publication bytes do
not change.

The qualified local index lives in `sessions-v1/`. Its filenames hash a
versioned length-prefixed agent/native identity; native IDs never become file
paths. A historical `sessions/` index is adopted only after its referenced
registration matches the archive ID, canonical agent and exact native ID.
New writers leave that legacy evidence in place and never write new
unqualified index entries. Retention and undo remove historical entries only
when they still name the removed registration.

## Recovery

Hooks read only directly referenced entries and registrations. A damaged index
never triggers a full registration scan on the user's turn. An eligible proven
start or Cursor transcript follow-up remains queued with its original capture
generation for the collector. Continuation evidence alone cannot admit a new
session.

The collector runs recovery on its first qualified-index pass and when a hook
requests it. Recovery enumerates registrations once per attempt, refuses
same-agent duplicate native identities, and atomically repairs unique entries.
An interrupted or unreadable census remains incomplete in `session-index.json`;
partial enumeration cannot prove an identity absent. Individually validated
registrations remain readable. A requested damaged identity is recorded absent
only after a complete census succeeds. Outstanding child candidates retain
their reserved archive IDs; they cannot recreate a missing parent.

Run `agent-archive sync` to retry collector recovery. Resolve unreadable or
conflicting local registration evidence using a complete backup before retrying.
A quarantined registration remains incomplete recovery evidence until it is
restored or deliberately resolved. Recovery's marker describes new-writer
local state; it does not detect arbitrary later filesystem loss. Bounded hooks
cannot discover unrelated manually introduced duplicate registrations.

## Running an older version

The migrated data home is not writable-compatible with older binaries. A new
format marker cannot make an already released binary refuse it: an old writer
may reuse a legacy collision or create another archive ID.

Before running an old writer, stop collectors and hooks and back up the data
directory. Restore the **complete pre-migration local snapshot**, accepting the
loss of later local pending work, or use a separate empty data home. Do not run
old and new writers together against the migrated home, and do not convert the
index back into an unqualified namespace. Existing remote archives remain
readable by older readers because their IDs, keys and schemas are unchanged.
