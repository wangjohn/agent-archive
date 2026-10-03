# Set up capture

Before enabling capture, read [what leaves your machine](../security/privacy.md#what-is-uploaded): filtering and credential redaction are best effort, there is no client-side encryption, and visible user-level `SKILL.md` text may be uploaded even when you include only one project. Setup captures new sessions in explicitly included projects; [backfill](../guides/backfill.md) imports older sessions only when you choose it.

```sh
agent-archive setup
```

Setup has three steps: choose apps and projects, connect storage, then review
and start. You need a private Cloudflare R2 or Amazon S3 bucket: an existing
one ([create one](bucket.md); [bucket permissions](../security/bucket-permissions.md)
has the least access it needs), or, for S3, one setup creates for you in
your own AWS account. Run it from inside a project you want
archived, so setup can offer it, or from anywhere: setup then offers the
projects your apps have sessions in. Choose "Show setup instructions" at the
storage prompt for a link to the [bucket guide](bucket.md). Setup asks questions, so it needs a terminal: without one it
stops before asking anything and changes nothing. Its prompt sequence is not
a scripting API; to script it, use [`setup --yes`](#set-up-without-questions).

## Before the first question

Setup first checks what starting to archive will need, and prints one line
for each check: a ✓, or a ✗ with the problem and how to fix it.

- **Hook files.** The file of each app setup found, or that your saved
  settings or an unfinished setup include, must be one setup can add its
  hooks to: plain JSON, without comments, trailing commas, or duplicate
  keys. A problem names the file, line, and column, such as
  `~/.claude/settings.json:2:3`. A file that doesn't exist yet is fine;
  setup creates it.
- **Background job.** The scheduler must say whether the collector's job is
  already loaded: `launchctl` on macOS, and on Linux `systemctl --user`, which
  needs a systemd user manager you can reach and systemd 240 or newer
  ([setup on Linux](#setup-on-linux)).
- **Keychain** (macOS) **or credentials file** (Linux). When your saved
  settings or an unfinished setup store in R2, the place where the R2 key is
  kept must open: the macOS Keychain, or on Linux the private credentials file
  folder ([where credentials are kept](../security/privacy.md#where-credentials-are-kept)).

A ✗ stops setup before it asks anything: nothing is changed, and an
unfinished setup is kept. Fix what is marked, then run `agent-archive setup`
again. To leave out an app whose file you don't want to change, choose the
apps with [`setup --yes --apps`](#set-up-without-questions). `setup --yes`
makes the same checks, for the apps it would include, and checks the
credential store (the Keychain, or on Linux the credentials folder) when it
stores in R2.

Setup captures only **new** sessions in the projects you include. To import
conversations already on this machine, run [`agent-archive
backfill`](../guides/backfill.md) afterwards.

## 1. Apps and projects

The first time, when you run setup inside a Git project and it finds apps,
it asks one question for both: "Archive Codex and Claude Code sessions in
~/src/web-app?" Accept to continue: the apps and that project are chosen, and
retention is the default 90 days. If your apps have sessions in other
projects, setup says how many; the review at the end repeats that "Edit a
setting" adds projects, drops apps, or changes retention. Decline to answer
the questions below instead. A repository that is your home folder or holds
it (a dotfiles checkout, say), or that is one of the temporary folders
backfill skips (`/tmp`, `/private/tmp`, `/var/folders`, `$TMPDIR`) or holds
one, is too broad to archive on one Enter: setup says so in one line, offers
no project, pre-selects none below either, and asks for the projects. A
repository inside a temporary folder, such as `/tmp/x`, is an ordinary project.
While it reads your apps' history for the count of other projects, setup
shows "Looking for your other projects...".

Otherwise setup offers the apps it finds together: "Include Codex and Claude
Code?" Accept to continue, or decline to choose apps individually. If no apps are
found, it opens the individual choices immediately. On reconfiguration, it
lists the apps included and not included, then asks "Change which apps are
included? [y/N]".

Setup then lists projects to archive: the ones Claude Code and Codex
sessions on this machine ran in, most recent first, with how many sessions each
has and when one was last used. Enter their numbers (`1 3`, or a range such
as `2-4`), `a` for all of them, or type a project path; a blank line
finishes. With no history to offer, it asks for paths.

If you run setup inside a Git project, that project heads the list, marked
✓ as already included, so a blank line archives just it. Here a number
switches a project in or out: enter `1` to leave the current project out.

Include each project explicitly; nothing outside an included project is
captured. If you finish with no project included, setup asks again.

## 2. Storage

Setup suggests S3 when your shell sets `AWS_PROFILE` or your AWS settings
already have a profile with credentials, and R2 otherwise.

The menu has two numbered choices: **Cloudflare R2** and **Amazon S3**.
After choosing a provider, press Enter to continue with bucket creation, or
choose **Use an existing bucket** before supplying creation credentials.
R2 creation still requires `AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1`; without
it, R2 guides you directly to connecting an existing bucket.

For R2 creation, supply a temporary setup token. Setup checks archive-key
permission access before showing a suggested name and automatic location.
For S3, select a creation profile; its region supplies the default, or setup
asks for a region if it has none. Both flows show a summary before creating
anything. Press Enter to create, **Customize** to change the settings, or
**Back** to return. Existing-bucket access remains available on the summary
and during error recovery. See the [bucket guide](bucket.md) for credentials
and permissions.

S3 setup enables and reads back all four Block Public Access settings. It
prints a narrower runtime policy, but does not create IAM identities or keys.
The creation profile remains the default archive profile; choose **Choose
another archive profile** to switch after attaching the policy yourself.

To connect an existing R2 bucket, enter its account ID, bucket and S3 keys.
Pasting `https://<account-id>.r2.cloudflarestorage.com/<bucket>` fills in both
account and bucket. Other R2 endpoints, including EU jurisdiction endpoints,
work too. Secret input is hidden on terminals and saved in the macOS Keychain
or a private credentials file on Linux.

For existing S3 storage, select an AWS profile and a bucket. Setup lists
buckets when allowed, looks up the bucket's region, or asks for these values
when your profile cannot read them. Profile discovery reads AWS configuration
only; it does not run credential processes or sign in.

When editing an installed configuration, **Keep current storage** is the
default. It still verifies the connection. Changing the destination opens the
provider flow. Saved incomplete setup drafts resume their existing step and
credentials; they do not automatically create a replacement bucket.

For scripted interactive answers, `r2-existing` and `s3-existing` jump directly
to the existing-bucket form. `r2-create` (when enabled) and `s3-new` jump to
creation. Bare `r2` and `s3` now open the provider flow. Noninteractive
`setup --yes` flags retain their existing behavior.

Creating a bucket needs `s3:CreateBucket` and
`s3:PutBucketPublicAccessBlock`, which the [runtime
policy](../security/bucket-permissions.md) deliberately does not grant, so
use a profile that has them for this step. If the profile is refused,
setup says which permissions are missing (an organization policy can also
forbid creation) and offers retry, profile replacement, or an existing bucket. If S3 gives no
clear answer to the request, setup says the bucket may exist and to check
the S3 console; it deletes nothing in that case. If the bucket was created
but Block Public Access could not be turned on, setup does not use it: it
offers to try again, to delete the empty bucket (after you type its name),
or to stop. When it succeeds, setup prints the runtime policy for the new
bucket (not for a folder name with characters other than letters, digits
and `. _ - /`; it says so and points at the guide) and asks which AWS
profile archiving should use. The profile that created the bucket is the
default, and it is usually far broader than archiving needs: attach the
policy to a separate IAM identity, save it as its own profile, and choose
that profile here to archive with less. The usual storage check then
verifies whichever you chose. If you choose storage again later in the same
run, setup offers the bucket it already created instead of making another,
and if setup ends without using a bucket it created, it says so and that
the bucket is empty, so you can delete it (or, when your saved setup draft
still names it, that running setup again resumes with it). Guided creation is interactive
only; `setup --yes` still takes an existing bucket.

Setup checks the connection in two steps. (After guided **Cloudflare R2** creation, setup has already made this check on the new key before storing
it, so a key that doesn't work is revoked at once; the check then runs again on
the stored key.) First it lists at most one object
under `.setup-test/`, which writes nothing, so a wrong account ID, key, or
profile fails within moments with an explanation. Then it writes one
temporary synthetic object (`.setup-test/<random>.json`), reads it back, and
deletes it. That proves the credentials work; it does not prove the bucket is private. Setup then
inspects the bucket's public-access settings read-only (S3 only; see
[privacy](../security/privacy.md#bucket-privacy-evidence)). R2 keys cannot
read those settings. For an existing R2 bucket, the review reminds you to
check public access in the Cloudflare dashboard. For a bucket setup just
created, it shows what the temporary Cloudflare token checked at setup.

Never pass secrets as command arguments; secret input fails rather than
falling back to visible keystrokes.

## 3. Review and start

The review lists what setup will save, then a checklist of what it found:

```
Step 3 of 3 · Ready to start

  Apps       Codex 0.121.0 · Claude Code 2.1.90
  Projects   ~/src/web-app
  Skills     metadata  User skill roots outside selected projects may be scanned
  Storage    s3://team-archive/agent-archive/  us-east-1 · profile work
  Keep for   90 days

  ✓ Storage connected       write, read, list, delete
  ✓ Bucket is private       all public access blocked
  ✓ Hook files are valid    ~/.codex/hooks.json, ~/.claude/settings.json
  ! Codex needs one step    approve the hooks with /hooks after setup
```

The summary shows the apps with their versions ("version not detected"
when setup could not read one), the projects, the storage address with its
region and profile (or R2 account), and the deletion period. The session
scope shows when it is not the default (every new session). An app you left
out is listed as skipped: setup does not offer it again, but you can add it
back under "Apps and projects" in a later `agent-archive setup`. The
deletion period is 90 days by default; older sessions are deleted from the
bucket automatically. When you reconfigure, each changed value is marked `*`
with its old value beneath it.

The Skills row controls filesystem skill evidence. Fresh setup uses
`metadata` (names and filtered hashes, no body). Choose Edit a setting to
select `none` or `body`. User-level skill folders may be scanned outside
your included projects. If an older configuration did not record a mode,
setup displays `body (kept from previous setup)` and lets you change it.
Changing the mode affects future uploads; it does not remove earlier local
or bucket copies.

In the checklist, ✓ is fine, ! needs you, and ✗ needs fixing first. While
any row is ✗, setup does not offer to start: fix what it names, then choose
"Check again", which checks storage and the hook files again.


- **Storage connected**: the storage check wrote, read, listed and deleted a
  test file.
- **Bucket is private**: for S3, all bucket-level Block Public Access settings
  were observed on. For a newly created R2 bucket, `r2.dev` was off and no
  custom domains were enabled when setup checked. An existing R2 bucket, or
  one whose settings could not be read, is marked !. A public bucket is marked
  ✗ unless you chose **Continue anyway** during guided R2 creation; that
  choice remains visible as a warning in the review.
- **Hook files are valid**: setup can edit each app's hook file.
- **Codex needs one step**: Codex asks you to approve new hooks. After
  setup, run `/hooks` in Codex and approve them.

Any warnings about the change follow, such as a shorter retention period or
hooks moving to another file.

At "Start archiving?", enter the number for "Edit a setting" to adjust apps, projects, session scope, retention, storage,
the folder inside the bucket, or the AWS region. The folder inside the
bucket (the prefix) is `agent-archive/` unless you change it. Retention is
a whole number of days from 1 to 36,500; there is no "keep forever" (36,500
days is about a century). Storage changes are checked
again before starting. If the connection test fails, setup says why and
how to fix it; see [when setup's storage check
fails](../guides/troubleshooting.md#when-setups-storage-check-fails).

Run setup from the `agent-archive` you will keep using. Setup refuses a
binary in a temporary folder, including the one `go run` builds and deletes
when it exits, since every hook and the collector would run a path that is
gone; build one with `go build` or use the installer.

Setup saves non-secret choices after each completed step. If it is
interrupted, run it again to continue or start over. After a plain
`agent-archive uninstall`, setup sets up again with your saved answers as
the defaults. Reconfiguration lets you
edit capture, storage, or retention separately (its headings then drop
"Step n of 3"), or choose "Nothing, exit" to leave without changing
anything, and keeps the machine identity, existing project activation
times, paused state, and unrelated hooks.

## Set up without questions

For a second machine, or any scripted setup, pass the answers as flags with
`--yes`. Setup then asks nothing, runs the same
[checks](#before-the-first-question) and storage check, and saves;
if an answer is missing or the check fails, it says so and changes nothing.
A bucket that allows public access stops it the same way, marked ✗ with a
link on fixing it, as it blocks the review of interactive setup; a bucket
whose public-access settings could not be read is only marked !.
Every missing or wrong answer is listed at once, one per line with the flag
that fixes it, before any check runs.

```sh
export AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY=...   # or pipe it on standard input
agent-archive setup --yes --provider r2 --r2-account ACCOUNT_ID --bucket BUCKET \
  --r2-access-key-id KEY_ID --project ~/code/app --project ~/code/api \
  --apps codex,claude

agent-archive setup --yes --provider s3 --bucket BUCKET --aws-profile PROFILE \
  --project ~/code/app
```

- The R2 secret access key comes from `AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY`,
  or else from standard input. It is never a flag. The access key ID can
  also come from `AGENT_ARCHIVE_R2_ACCESS_KEY_ID`. Setup removes both
  variables once read, so no command it runs inherits them. Without a new
  key, the one already stored is kept.
- If saving fails after the storage check, a new R2 key stays with the
  unfinished setup; run `agent-archive setup` to finish or discard it.
- `--r2-account` also takes the bucket URL, which names the bucket too.
- For S3, `--region` defaults to the profile's region, and must look like a
  region, such as `us-east-1`.
- `--apps` defaults to the apps already set up, else those found on this machine.
  It can add apps but never removes one: it must name every app already set
  up, and to remove an app (and its hooks) you run `agent-archive setup`.
- `--project` adds to the projects already set up; repeat it for several.
  `--project-repo REPO_KEY` matches a unique local clone by its origin's hash,
  independently of `--project`; ambiguous, excluded, or incomplete matches
  are skipped. See [repository matching](../guides/multiple-machines.md#repository-matching-in-setup-commands)
  for discovery limits.
- `--prefix PREFIX` sets the folder inside the bucket, including when changing
  only that setting on an existing installation.
- `--retention-days DAYS` sets retention from 1 to 36500 days.
- `--require-skill-use` captures only sessions that use skills;
  `--no-require-skill-use` captures sessions with or without skills. Omitted
  settings keep their saved values (fresh setup captures both).
- `--skill-evidence none|metadata|body` sets the skill evidence mode. A fresh
  setup defaults to `metadata`; an older configuration without the field
  retains `body` until changed.
- `--no-skills` installs no [agent skills](#what-setup-changes-on-your-machine)
  (such as `/handoff`) and removes the ones setup wrote earlier; a file that
  is not setup's is left alone and named. It works with or without `--yes`
  (without it, setup on a finished installation first asks what to change,
  so `agent-archive setup --yes --no-skills` changes only this). It is saved
  (`no_skills` in the [configuration](../reference/configuration.md)), and
  later setup runs keep the skills off. `--skills` turns them back on and
  installs them. Giving both is an error.
- Without storage flags, the storage already set up is kept, so
  `agent-archive setup --yes --project DIR` just adds a project.

[CLI reference](../reference/cli.md#agent-archive-setup) lists every flag.

## Refreshing after an upgrade

The hooks, the background job's definition (the LaunchAgent's plist, or the
systemd unit files), and the skill files name the
`agent-archive` binary and its flags, so a new binary needs them rewritten.
`agent-archive setup --refresh` does that, and nothing else: it asks
nothing, needs no terminal, and never touches storage, credentials,
projects, retention, or your saved answers. It prints `nothing to refresh`
(exit 0) when all is current, or one line saying what it refreshed
(`--verbose` lists the files). [The installer](install.md#install-with-the-script)
runs it for you when it finds a set-up machine.

- It uses the saved settings and the hook files setup recorded, and the
  executable you run it from. If the recorded executable differs (it moved,
  or was deleted, the case `status` calls "capture has stopped"), it points
  the hooks, the job's definition, and the skills at the running one and
  records it.
  It refuses a temporary build (`go run`) or a file that cannot run.
- Skills follow the same rules as in setup: a stale file of setup's is
  replaced, a file that is not setup's is left and named, and with
  `--no-skills` saved none are installed. It writes what setup would, so a
  skill or hook file you deleted by hand is written again (to keep the
  skills off, use `--no-skills`; to stop capture, use `agent-archive
  uninstall` or `pause`, not deleting the hooks).
- It changes the background job only when its definition itself changes (a
  moved binary) for a job that is loaded: that job is stopped and started
  again so it runs the new definition. A job that is not loaded stays that
  way, and an unchanged definition leaves the scheduler alone.
- All of it is one transaction with setup's journal, so a failure puts every
  file back, and a Ctrl-C or closed terminal while it writes does not stop
  it halfway. It waits up to ten seconds for a background collection pass
  that is running, then refuses and asks you to retry.
- It refuses, changing nothing and exiting 1 with one line on standard
  error, when setup never finished, an interrupted setup needs recovery, the
  archive was uninstalled, another installation's hooks are in a hook file
  it would write, another installation owns the background job, another
  setup is running, or it runs as root (`sudo`) in a home directory that
  belongs to another user, where it would leave root-owned files. Run it as
  yourself. Any other flag except `--verbose` is a usage error
  (exit 2).

## Setup on Linux

Setup is the same on Linux, with these differences ([what was tested, and
what was not](install.md#platforms)).

- **The background job is a systemd user timer.** Setup writes
  `agent-archive-collector.service` (a one-shot that runs `agent-archive
  _collect`, with its output appended to `collector.log` and
  `collector-error.log` in the data directory) and
  `agent-archive-collector.timer` (a minute after boot, so at once when setup
  starts it, then a minute after each start; in a container the first run can
  wait up to a minute) into `~/.config/systemd/user`, then runs
  `systemctl --user daemon-reload` and `enable --now` on the timer. It needs
  systemd 240 or newer (RHEL 8 and its rebuilds from 8.3); an older one is
  refused. There is no cron or other fallback: without a systemd user manager
  setup stops.
- **No user bus.** Over SSH without `pam_systemd`, in a container, or after
  `su`, `systemctl --user` cannot reach a user manager. Setup then stops at
  its first check, before it asks anything or changes anything:

  ```text
  Checking installed applications...
    ✓ Claude Code hooks: ~/.claude/settings.json
    ✗ Background job: the systemd user manager cannot be reached (this session has no user bus), so setup cannot tell whether the agent-archive-collector job is loaded, and loads it only when it can tell
      Run this from a login session that has a systemd user bus (a desktop or console login, or ssh with pam_systemd), or run `loginctl enable-linger` once so the user manager runs without a login, then check that `systemctl --user status` works, then run agent-archive setup again.
  Setup incomplete. Nothing was changed, and any unfinished setup is kept. Fix what is marked ✗ above, then run agent-archive setup again.
  ```

  After `loginctl enable-linger` the manager starts at boot, but a shell that
  was already open may still lack the bus: log in again, or `export
  XDG_RUNTIME_DIR=/run/user/$(id -u)`, and `systemctl --user status` should
  answer. Then run `agent-archive setup` again.
- **Lingering.** Without it, the user manager exists only while you are logged
  in, so the collector stops when you log out. Hooks keep recording sessions
  and the next pass after you log in uploads them, but nothing uploads while
  you are away. `status` and `status --json` (`background_warnings`) say so
  while the job is otherwise working; `loginctl enable-linger` fixes it. A
  server or a machine you reach over SSH wants lingering on.
- **Where the units go.** Always `~/.config/systemd/user`, whatever
  `XDG_CONFIG_HOME` your shell has. The user manager reads
  `$XDG_CONFIG_HOME/systemd/user` instead only when its own environment sets
  the variable (as `pam_env` or a `user@.service` drop-in can); a variable
  exported in your shell, the usual place, never reaches it. If the manager
  does look elsewhere, `enable --now` fails with "Unit file ... does not
  exist", setup puts everything back and the message names the directory the
  units were written to; `systemctl --user show --property=UnitPath` lists
  where the manager looks. Setup has no option to write the units elsewhere:
  unset the variable for the manager (look in `/etc/environment`, `pam_env`
  and any `user@.service` drop-in), log in again, and run setup again.
- **`XDG_CONFIG_HOME` and `XDG_CACHE_HOME` are forwarded to the job** when
  your shell sets them to an absolute path, so backfill from your shell and
  the scheduled collector find the same Cursor database and keep Cursor
  database copies in the same place (`~/.cache/agent-archive/cursor-snapshots`
  by default, never `/tmp`). A relative or empty value counts as unset.
  `status` warns when the shell's value later differs from the job's.
  **A limit to know about:** a systemd user manager can get these two
  variables from outside your shell (an `environment.d` file, a desktop
  session's `import-environment`). Setup records only what the shell has, so
  if the shell had neither variable when setup ran, the job uses whatever
  the manager has. `status` compares only your shell's values with the ones
  recorded and cannot see the manager's, so it cannot warn that the job and a
  backfill from your shell look in different places. The workaround is to export the same values in
  the shell as the manager has, and run `agent-archive setup` again, which
  records them.
- **The job's `PATH`** is your shell's usable `PATH` entries when setup ran,
  then systemd's own default (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`),
  so a `credential_process` helper such as `aws-vault`, `op` or one in
  `~/.local/bin` is found. A directory that does not exist when setup runs
  is not recorded; run setup again after installing a helper there.
- **The job's working directory is `/`.** Use an absolute path or `~/` for
  a `credential_process` helper under your home. A path such as
  `./bin/helper` is resolved from `/`, as it is for the macOS collector.
  Existing installations get this setting when you run setup again.
- **Credentials.** There is no Keychain. An R2 key is kept in a private file
  that is not encrypted, and an S3 profile stores no secret of
  agent-archive's own; prefer S3 on Linux. In a container or a service, set
  `AGENT_ARCHIVE_R2_ACCESS_KEY_ID` and `AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY`
  instead. See [where credentials are kept](../security/privacy.md#where-credentials-are-kept).
- **Cursor** is best effort, and setup cannot detect its version (it says
  "version not detected"); see [what is not verified](install.md#platforms).
- **Moving or copying.** A data directory copied to another machine, or a
  cloned VM or container image, makes `status` and `setup` warn that it was
  set up on a different machine; see [cloned machines on
  Linux](../guides/multiple-machines.md#cloned-machines-on-linux).
- **A network home.** If the data directory or `~/.config/systemd/user` is on
  a network filesystem (NFS, SMB/CIFS, sshfs and the like), setup refuses
  before its first question and changes nothing: machines sharing a home would
  share one machine ID, cannot rely on file locks and would each run the
  collector. Put the data directory on local disk with `AGENT_ARCHIVE_HOME`,
  or, for a home only one machine mounts, run `agent-archive setup
  --allow-network-home`, which is recorded in `config.json` and does not stop
  `status` warning. See [a home directory shared across
  machines](../guides/multiple-machines.md#a-home-directory-shared-across-machines).
- **Uninstalling without a user bus.** `agent-archive uninstall` refuses
  when the manager cannot say whether the job is loaded;
  `uninstall --skip-scheduler` removes the units, the hooks and the skill
  files anyway and prints the `systemctl --user stop` command to run from a
  session that has the bus ([uninstall](uninstall.md#when-the-background-scheduler-cannot-be-reached)).

## What setup changes on your machine

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
- **A background job** that runs the collector every 60 seconds: on macOS a
  LaunchAgent, `~/Library/LaunchAgents/com.agent-archive.collector.plist`; on
  Linux a systemd user timer and service,
  `~/.config/systemd/user/agent-archive-collector.timer` and `.service`, each
  mode 0600, plus the link `systemctl --user enable` makes in
  `timers.target.wants` ([setup on Linux](#setup-on-linux)). The scheduler
  gives it none of your shell's environment, so for S3 its definition (the
  plist, or the service's `Environment=` lines) also carries what setup's
  storage check ran with: `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`,
  `AWS_CA_BUNDLE`, the `AWS_ENDPOINT_URL` overrides, the proxy
  variables (a proxy URL with a password is left out), and aws-vault's and
  1Password's settings for where credentials live, when set, and your
  shell's `PATH` (without directories every account can write to), which a profile's `credential_process`
  (`aws-vault`, `op`, `granted`) is found through. Never your shell's AWS
  keys or tokens. If that command is not a program on your `PATH` (an alias
  or shell function), the review warns that background uploads will fail.
  Run setup again after moving your AWS files or the helper; `status` warns
  when they no longer match
  ([configuration](../reference/configuration.md#environment-variables)).
- **Agent skill files**, one folder per skill under each app's skills
  directory: `/handoff` is written to `$CLAUDE_CONFIG_DIR/skills/handoff/SKILL.md`
  (`~/.claude/skills/…` by default) for Claude Code and to
  `~/.agents/skills/handoff/SKILL.md` for Codex and Cursor
  ([handoff](../guides/handoff.md#from-inside-an-agent-handoff)), and
  `agent-archive`, which lets the agent find and pull in a past session, to
  `skills/agent-archive/SKILL.md` in the same two places
  ([agent skills](../guides/agent-skills.md)). Each file
  carries a marker line: setup replaces, `status` lists, and uninstall
  removes only a file with it (naming this installation's data directory),
  and leaves any other file at that path alone, saying so. After you upgrade
  `agent-archive`, `status` warns about a skill file written by an earlier
  release; [`agent-archive setup --refresh`](#refreshing-after-an-upgrade)
  refreshes it (the installer runs that). Setup installs them without
  asking and says how to opt out: `agent-archive setup --no-skills` removes
  the files it wrote and keeps them off in later runs (`status` says they are
  turned off), and `agent-archive setup --skills` turns them on again.
- **Local state** in `~/.local/share/agent-archive` (or `AGENT_ARCHIVE_HOME`;
  see [local state](../reference/local-state.md)), private to your account.
  It holds registrations, frozen uploads, and caches; transcripts are read in
  place, not copied, except a Cursor database copy that exists only while a
  read of it is in progress.
- **A Keychain item** (service `agent-archive`) for R2 credentials on macOS;
  on Linux, a credentials file (mode 0600, in a mode 0700 folder) in the data
  directory. S3 credentials stay in your AWS profile.

`agent-archive uninstall` removes the hooks, the skill files, and the
background job (the LaunchAgent, or the systemd units and their enable link);
`--delete-local-data` also removes the local state and the Keychain item
(or credentials file).
Neither touches the bucket ([uninstall](uninstall.md)).

Only the account's own default installation, in `~/.local/share/agent-archive`
under the home directory the system records for your account, uses the
job name `com.agent-archive.collector` (launchd) or `agent-archive-collector`
(systemd). Any other data directory, whether set
with `AGENT_ARCHIVE_HOME` or moved by a sandbox that overrides `HOME`, gets a
name of its own (`com.agent-archive.collector.<hash>`, or
`agent-archive-collector-<hash>` under systemd), and its hooks carry
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
too. Before stopping a job, setup and uninstall check that the scheduler loaded
it from this installation's own definition (plist or unit file); a job loaded
from any other one is left running and reported. These keep a second or test
installation from stopping or replacing the default one. They do not stop it
from loading its own job into your real launchd or systemd user manager: stub
`launchctl` or `systemctl` in tests (see
[testing](../../dev/contributing/testing.md)).

## After setup

After "Configuration saved.", setup says, with one line per app, what to do next:

- **Codex:** run `/hooks` and approve the archive hooks, then start a new
  session. Codex doesn't run hooks it hasn't approved, and this is the most
  common reason nothing is captured.
- **Claude Code:** nothing to approve; start a new session.
- **Cursor:** nothing to approve; start a new Agent chat.

Setup's closing lines also name the agent skills it installed (`/handoff` and
`agent-archive`) and the opt-out, `--no-skills`. Unlike Codex's hooks, a skill
needs no approval; Claude Code and Codex notice it in a session that is
already open (Claude Code needs `/reload-skills` when it had no
`~/.claude/skills` folder at start), and Cursor may need a new chat. Then ask an agent in words, such
as "pull in the auth session from Codex" ([agent skills](../guides/agent-skills.md)).
Claude Code asks before it first uses the skill and before the commands it
runs.

Sessions already open are not captured: capture needs a provable fresh
start, so only a new session in an included project counts. In Codex and
Claude Code, `/clear` also starts one; in Cursor, only a new chat does. Setup finishes without waiting for it. Check
progress with:

```sh
agent-archive status
agent-archive status --json
```

What each status line means is in
[troubleshooting](../guides/troubleshooting.md#reading-status).

Once it has said how to check capture, setup offers to import the past
sessions of the projects you chose, when they have some on this machine that
aren't in the archive yet:

```text
Looking for past sessions in these projects… 214 found.
Import the 214 past sessions from these projects? [Y/n]
```

Yes runs the same import as `agent-archive backfill --project DIR` for each
chosen project, with the same checks, and uploads the sessions; it ends with
the import's ID, and `agent-archive backfill undo ID` removes them again (see
[backfill](../guides/backfill.md)). If the import stops or fails, setup
stays done and prints the `agent-archive backfill` command that finishes
it. No changes
nothing; you can run `agent-archive backfill` any time. Setup skips the offer
while capture is paused, when there is nothing to import, and with `--yes`,
which asks nothing and mentions `agent-archive backfill` in its next steps
instead.

Setup's storage check and installed hooks establish configuration, not a captured session. After `agent-archive sync` or the next background pass, check that the app's Capture row says **archived, verified**, then confirm the session appears in `agent-archive list` and `agent-archive show SESSION_ID`. An overall `Ready` state alone does not establish that this app published a new session and had it read back. For a short route through the check, see [first successful capture](../README.md#first-successful-capture).

Setup's last line, after the import offer, is the command that sets up another machine with the same
storage, capture rules, agent skill installation policy, apps and projects
([without questions](#set-up-without-questions)),
ready to copy:

```text
To set up another machine with this storage, run there:
  agent-archive setup --yes --provider s3 --bucket BUCKET --aws-profile PROFILE --region us-east-1 --apps codex,claude --prefix agent-archive/ --retention-days 90 --no-require-skill-use --skill-evidence metadata --skills --project ~/code/app
```

Whole repositories with a known origin use `--project-repo` for bounded
[repository matching](../guides/multiple-machines.md#repository-matching-in-setup-commands).
Other project paths in your home folder are written from `~`; adjust those
paths for the new machine. For R2 the command never
carries the key: set `AGENT_ARCHIVE_R2_ACCESS_KEY_ID` and
`AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY` on the other machine first. The command
carries the saved folder with `--prefix`, retention, skill-use capture rule,
effective skill evidence mode, and whether agent skills are installed.
Omitting these flags from a scripted reconfiguration keeps the saved settings.

For another machine, the encrypted shared-key beta can transfer settings through
`setup --pair` or `setup --pair-file PATH`. Read [Multiple machines](../guides/multiple-machines.md)
for separate bundle/code delivery, destination consent, scope review, and the
shared R2 key's revocation limit. Pairing refuses to run inside a coding agent.

When your capture scope contains exclusions, the second-machine command uses
`--project-scope JSON` to transfer all inclusion and exclusion rules together.
Repository-relative exclusions and reincluded subtrees follow a relocated
checkout. Missing excluded folders stay excluded if created later; a failed
repository match or unsafe subtree mapping refuses the transfer before
changing capture settings. Saved destination exclusions stay in force; conflicting
saved reinclusions refuse the transfer until you review the destination scope.
Review non-home absolute paths for the new machine.
