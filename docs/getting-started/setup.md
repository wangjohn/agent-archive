# Set up capture

```sh
agent-archive setup
```

Setup has three steps: choose apps and projects, connect storage, then review
and start. You need an existing private Cloudflare R2 or Amazon S3 bucket
([create one](bucket.md); [bucket permissions](../security/bucket-permissions.md)
has the least access it needs). Run it from inside a project you want
archived, so setup can offer it. Type `help` at the storage prompt for
provider instructions. Setup asks questions, so it needs a terminal: without one it
stops before asking anything and changes nothing. Its prompt sequence is not
a scripting API.

Setup captures only **new** sessions in the projects you include. To import
conversations already on this Mac, run [`agent-archive
backfill`](../guides/backfill.md) afterwards.

## 1. Apps and projects

Setup offers the apps it finds together: "Include Codex and Claude Code?"
Accept to continue, or decline to choose apps individually. If no apps are
found, it opens the individual choices immediately. On reconfiguration, it
lists the apps included and not included, then asks "Change which apps are
included? [y/N]".

If setup finds the current Git project, it shows its full path and asks
"Archive sessions in this project?" Accept, or decline to enter project paths
yourself. Include each project explicitly; nothing outside an included
project is captured.

## 2. Storage

- **R2:** enter a bucket, an account ID or S3 endpoint, and credentials.
  Secret input is hidden on a terminal and stored in the macOS Keychain.
- **S3:** enter the bucket and choose an existing AWS profile. Setup offers
  the profiles in your AWS settings and uses the profile's region when it has
  one; it asks for a region only when one is missing.

Setup checks the connection with one temporary synthetic object
(`.setup-test/<random>.json`), which it deletes again. That proves the
credentials work; it does not prove the bucket is private. Setup then
inspects the bucket's public-access settings read-only (S3 only; see
[privacy](../security/privacy.md#bucket-privacy-evidence)).

Never pass secrets as command arguments; secret input fails rather than
falling back to visible keystrokes.

## 3. Review and start

The summary shows the apps, projects, destination, session scope, and
automatic deletion period (90 days by default; older sessions are deleted
from the bucket automatically). At "Start archiving?", enter the number for
"Edit a setting" to adjust apps, projects, session scope, retention, storage,
the folder inside the bucket, or the AWS region. The folder inside the
bucket (the prefix) is `agent-archive/` unless you change it. Retention is
a whole number of days from 1 to 36,500; there is no "keep forever" (36,500
days is about a century). Storage changes are checked
again before starting. If the connection test fails, choose "Edit settings"
or "Retry" after restoring access.

Filtering is best effort, so archived text can still contain sensitive
information. Read [what leaves your Mac](../security/privacy.md) before
enabling.

Setup saves non-secret choices after each completed step. If it is
interrupted, run it again to continue or start over. Reconfiguration lets you
edit capture, storage, or retention separately, and keeps the machine
identity, existing project activation times, paused state, and unrelated
hooks.

## What setup changes on your Mac

- **Hooks**, where each app reads them: Claude Code's
  `$CLAUDE_CONFIG_DIR/settings.json` and Codex's `$CODEX_HOME/hooks.json`
  when those variables are set in the shell you run setup from, otherwise
  `~/.claude/settings.json` and `~/.codex/hooks.json`; Cursor's is always
  `~/.cursor/hooks.json`. The paths are recorded (`hook_files` in the
  [configuration](../reference/configuration.md)), so `status` and
  `uninstall` find them from any shell. Only the `hooks` entry of each file
  is rewritten (and Cursor's `version`, when missing): every other setting
  keeps its exact text, key order, and numbers, and uninstall restores a file
  setup only added hooks to byte for byte. A file that removing the hooks
  leaves empty (`{}`, or Cursor's `version` alone), as when setup created
  it, is deleted. A file that is a symlink, as dotfile managers such
  as stow or chezmoi create, is updated at its target and the link is kept.
  Setup refuses a file it cannot edit safely and says where the problem is:
  anything that is not plain JSON (a comment, a trailing comma, a byte-order
  mark), or a key that appears twice inside `hooks`.
- **A LaunchAgent**, `~/Library/LaunchAgents/com.agent-archive.collector.plist`,
  which runs the collector every 60 seconds.
- **Local state** in `~/.local/share/agent-archive` (or `AGENT_ARCHIVE_HOME`;
  see [local state](../reference/local-state.md)), private to your account.
  It holds registrations, frozen uploads, and caches; transcripts are read in
  place, not copied, except a Cursor database copy that exists only while a
  read of it is in progress.
- **A Keychain item** (service `agent-archive`) for R2 credentials. S3
  credentials stay in your AWS profile.

`agent-archive uninstall` removes the hooks and the LaunchAgent;
`--delete-local-data` also removes the local state and the Keychain item.
Neither touches the bucket ([uninstall](uninstall.md)).

Only the account's own default installation, in `~/.local/share/agent-archive`
under the home directory macOS records for your account, uses the launchd
label `com.agent-archive.collector`. Any other data directory, whether set
with `AGENT_ARCHIVE_HOME` or moved by a sandbox that overrides `HOME`, gets a
label of its own (`com.agent-archive.collector.<hash>`), and its hooks carry
the directory in their command, since apps run hooks without your shell's
environment. That directory is also how each installation recognizes its own
hooks: setup replaces and uninstall removes only handlers whose command runs
with this installation's data directory, and leaves another installation's
alone. If an app's hook file already holds another installation's hooks,
setup names that installation before its first question and installs
nothing beside it (two installations would each capture every session): if
you choose that app, setup stops right after the apps and projects step,
before asking about storage, and keeps your answers. `status` reports it too;
uninstall that installation first, or give the new one its own `HOME` (or
`CLAUDE_CONFIG_DIR` and `CODEX_HOME`) so the apps' hook files are separate
too. Before stopping a job, setup and uninstall check that launchd loaded it
from this installation's own plist; a job loaded from any other plist is left
running and reported. These keep a second or test installation from stopping
or replacing the default one. They do not stop it from loading its own job
into your real launchd: stub `launchctl` in tests (see
[testing](../contributing/testing.md)).

## After setup

Approve the hooks in each app if it asks (Codex CLI: `/hooks`): an app
doesn't run hooks it hasn't approved, and this is the most common reason
nothing is captured. Then start a harmless new session in an included
project. Setup finishes without waiting for it. Check
progress with:

```sh
agent-archive status
agent-archive status --json
```

What each status line means is in
[troubleshooting](../guides/troubleshooting.md#reading-status).
