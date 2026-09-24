# Session start eligibility

A session is eligible only after its project has been included.

A new `SessionStart` must prove that it begins a conversation rather than resuming one that may predate activation. There are two proofs.

The first is the documented source. Codex and Claude Code provide `source: startup` or `source: clear` for a conversation that begins now; `resume` and `compact` do not establish the start of a previously unseen session, and neither does any other value. When a Codex or Claude payload carries a `source`, it decides, and the second proof cannot override it.

The second is the transcript itself: at the true start of a conversation the hook-provided `transcript_path` names a file that does not exist yet or holds no bytes, while a resumed conversation points at a transcript that already has content. It is the fallback when a Codex or Claude payload carries no `source` at all; for those, a start with no `transcript_path` proves nothing and stays declined, as does one whose path is not absolute or whose transcript cannot be stat'd. A version string is never proof of anything. The transcript is stat'd, never opened, and its path is never recorded in a diagnostic.

### Cursor

Observed on the Cursor desktop app 3.21.13, from Cursor's own Hooks output:

- A new Agent chat fires **no `sessionStart`**. Its first hook is `beforeSubmitPrompt`, then `afterAgentResponse` and `stop`.
- On a new chat's first `beforeSubmitPrompt`, `transcript_path` is `null`. `afterAgentResponse` and `stop` then carry `~/.cursor/projects/<workspace>/agent-transcripts/<id>/<id>.jsonl`.
- On a resumed chat, the first `beforeSubmitPrompt` already names the existing, non-empty transcript. `sessionEnd` can fire mid-turn.
- Every payload carries `conversation_id`, `session_id` (the same value), `cursor_version`, `workspace_roots`, `composer_mode`, `model`, `model_id`, and `model_params`.

So a never-seen Cursor conversation is registered at its first `beforeSubmitPrompt` (and still at `sessionStart`, should a version fire one) when `transcript_path` is null, absent, or names an absolute path to a missing or empty file. A transcript that already has bytes is a resume and is declined with the `session_start_unknown` diagnostic, for included projects only. `session_started_at` is the time of that first hook. A registration made without a path takes it from a later `beforeSubmitPrompt`, `afterAgentResponse`, or `stop`, but only a path that is absolute and whose file name is `<conversation_id>.jsonl`; a path, once set, is never replaced. Until the path arrives the collector treats the session as waiting, not failed. The same holds once the path names a file that exists but still holds no bytes: nothing has been captured, so the pass records no error, keeps any queued request, and reads the file again next time.

Unverified on 3.21.13, and kept fail-closed:

- No `sessionStart` fired for a desktop chat in the live check. The `sessionStart` rule is kept for a version that does fire one and has not been exercised against one.
- `subagentStart` and `subagentStop` were not observed.
- The file-name rule compares the path against the registered native id, which is `session_id`; on 3.21.13 that equals `conversation_id`. A version that separates them would never have its transcript adopted, and the chat would wait.
- Whether the transcript already holds the turn's records at the moment `afterAgentResponse` names it was not checked. The collector re-reads on every pass, so a late write is picked up by the next one.
- No session from 3.21.13 has been published and read back; support stays `unverified`.

This fails closed. A resumed chat is never mistaken for a new one, because its first prompt names its existing transcript. If transcripts are disabled, every hook's path is null: the chat registers but never receives a path, so nothing is read or uploaded. Its registration and queued hook evidence stay local, and retention does not currently expire a session with queued hook evidence, so they remain until the evidence is published or cleared (an open item in the implementation ledger). Codex and Claude Code are unchanged: a prompt from a session that never started registers nothing.

Project identity is matched by configured project, not by exact directory, for new registrations and continuations alike. A harness reports the session's working directory, which for a Claude Code worktree is `<project>/.claude/worktrees/<name>` and for a session started anywhere else in the tree is that subdirectory. The configured project that owns it — the nearest configured ancestor of the reported directory, comparing resolved paths — is the project the session registers under, and the registration stores that configured root spelling. Eligibility, the activation boundary, and the project ID all use it. The nearest ancestor wins, so a configured project nested inside another keeps its own inclusion decision rather than inheriting its parent's. A working directory that belongs to no configured project registers nothing and records nothing.

Already accepted registrations retain their original start time on resume. An absent transcript path does not erase the saved path. A different harness or project cannot replace the registration identity: a resume or compact from a subdirectory of the registered project is the same session, while a directory that belongs to a different configured project is a conflict.

For included projects, skipped starts (including a declined Cursor first prompt) produce a bounded local diagnostic with application, project, reason, and time. Three reasons exist: the project is not yet active, the start could not be established, and setup was still in progress. The last one covers the window in which `setup` holds an open transaction: hooks register nothing then, so a start that lands in that window says so instead of disappearing. Writing it does not wait for setup's lock, so it is best effort. Diagnostic records contain no transcript, native session ID, or transcript path. Excluded project paths are not recorded, and a recorded diagnostic is dropped once its project is excluded: `status` reports only currently included projects, and saving a setup that excludes a project prunes its stored entries. When a start is declined for more than one reason, the diagnostic names the most specific one: a project that is not yet active is reported as such even if the start was also a resume. `status` and `status --json` show these reasons; hooks remain local and return successfully to the agent.

## Imported sessions

`agent-archive backfill` registers sessions the hooks never saw, after the
person confirms a plan ([backfill spec](agent-archive-backfill-spec.md)). An
imported registration has `origin: import` and two times:

- `session_started_at` is the true start, from the transcript
  (`started_at_source: transcript`) or, for a Cursor file with no timestamps,
  the file's birth time (`file_created`).
- `admitted_at` is when backfill took ownership. A hook registration sets it
  to its own start.

Every boundary check uses `Admitted()`, which is `admitted_at`, or
`session_started_at` for a registration older than that field:

| Check | Uses |
|---|---|
| Project activation in `AcceptSession` | `Admitted()` |
| Storage destination in `AcceptSession`, and retention's current-bucket check (`InCurrentDestination`) | The registration's `destination_id`, set at registration by hooks and backfill; `Admitted()` against `DestinationSince` for a registration without one |
| Retention age before a first capture | `Admitted()` |
| App selection | `Harnesses`, plus `ImportedHarnesses` for imports |
| Fresh-start eligibility for hooks, metadata `started_at`, subagent ordering, handoff | `session_started_at` |
| App hook verification, `HookObserved`, skill inventory | hook-registered sessions only |

A guard test fails if code compares `session_started_at` with `ActivatedAt`
or `DestinationSince` outside `Admitted()`, and a second one if code compares
an admission with `DestinationSince` outside `InCurrentDestination`. A hook
that later resumes an imported session continues it: the registration keeps
its start, admission, destination ID, and origin. Retention and undo leave a removal record when they forget a
session, so backfill does not import it again.
