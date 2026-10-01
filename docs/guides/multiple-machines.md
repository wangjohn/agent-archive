# Using more than one machine

Several machines, macOS and Linux in any mix, can archive into the same
bucket and prefix. Each machine runs its own setup, with its own machine ID,
hooks, collector, and local state.

- **Ownership is per machine.** A session belongs to the machine that
  registered it. Only that machine uploads it, republishes it, and deletes it
  when retention expires or `backfill undo` removes it. Local state is never
  shared or synced between machines. So the sessions of a machine you retire
  or wipe stay in the bucket until you delete them; see
  [delete the archive](../getting-started/uninstall.md#delete-the-archive-in-the-bucket),
  which also shows a lifecycle rule that cleans up after a machine that is
  gone.
- **Reading is shared.** `list`, `show`, and `handoff` read the bucket, so
  they see every machine's sessions. Each session's metadata records the
  `machine_id` that captured it.
- **Handoff across machines.** `agent-archive handoff --latest` on another
  machine downloads the session from the archive. It matches the project only
  when the repository is checked out at the same path; otherwise use a session
  ID from `list`. Push your branch first: uncommitted changes stay on the
  machine that made them. See [handoff](handoff.md).
- **Retention is per machine too.** Each machine's retention setting applies
  to the sessions it owns. Set the same value on every machine if you want one
  policy.
- **Credentials.** Each machine needs its own access to the bucket: an AWS
  profile, or R2 credentials entered in its own setup (stored in that
  machine's Keychain on macOS, or in a private file under its data directory
  on Linux; see [where credentials are kept](../security/privacy.md#where-credentials-are-kept)).
  [`setup --yes`](../getting-started/setup.md#set-up-without-questions)
  sets up another machine from a script. A key limited to one prefix (see
  [bucket permissions](../security/bucket-permissions.md)) works for several
  machines sharing that prefix.

Don't copy the data directory from one machine to another: two machines that
believe they own the same sessions would publish over each other. The rest of
this page is about the ways that happens by accident.

## Migration Assistant and Time Machine (macOS)

Migration Assistant and a Time Machine restore copy
`~/.local/share/agent-archive` like any other folder, machine ID included.

- **Moving to a new Mac, retiring the old one.** Nothing to do: the new Mac
  carries on as the same machine and owns the old sessions. Rerun
  `agent-archive setup` if `status` reports hooks or background as broken
  (the binary moved).
- **Both Macs stay in use.** On the new one, run `agent-archive uninstall
  --delete-local-data` and then `agent-archive setup`, which gives it a new
  machine ID. Sessions captured before the move stay owned by the old Mac.
- **Restoring an older backup on the same Mac.** The data directory goes
  back to the backup's state: sessions registered after the backup are
  forgotten locally, so this Mac never updates or deletes their copies in
  the bucket. A [lifecycle rule](../getting-started/uninstall.md#a-lifecycle-rule-as-a-backstop)
  eventually removes them.

## Cloned machines on Linux

A cloned VM, a container image with a set-up home inside it, a restored
backup, or a copied home directory carries the data directory and so the
machine ID. The same advice applies as on macOS:

- **The original is retired, and the copy takes over.** Nothing to do for the
  sessions: the copy carries on as the same machine and owns the old
  sessions. The systemd units live in `~/.config/systemd/user`, outside the
  data directory, so a copy of the data directory alone has no background
  job: run `agent-archive setup` if `status` says the background collector
  is not running (`setup --refresh` when it reports it as broken because the
  binary moved).
- **Both stay in use.** On the copy, run `agent-archive uninstall
  --delete-local-data`, then `agent-archive setup`, which gives it a new
  machine ID. Sessions captured before the clone stay owned by the original.

Linux has a cheap signal, so there is a warning. The first setup records a
digest of the machine's own ID (`/etc/machine-id`, else
`/var/lib/dbus/machine-id`) in `config.json` as `host_id`
([configuration](../reference/configuration.md)); it is never uploaded.
`status` and `setup` warn when the data directory was set up on a machine
whose ID is not this one's, which is what a clone whose machine ID was
regenerated looks like (cloud images, `sysprep` and most VM templates do
that, and it is the usual case). The warning names the two ways out: on a
copy whose original is still in use, `uninstall --delete-local-data` and
`setup` as above; when the original is retired, or this is the same machine
with its operating system reinstalled over the same home, remove the
`host_id` entry from `config.json` and the warning stops (setup records the
current machine's the next time it runs).

A data directory copied from a Mac to a Linux machine carries no `host_id`
(macOS records none), so nothing warns; treat it as a clone and set up again.

The warning is best effort, so set up a cloned machine again whether or not
it fires:

- It cannot see a clone that kept its machine ID (a disk copied as it is, a
  container committed with `/etc/machine-id` baked in).
- It says nothing when this machine has no usable ID: an image's empty
  `/etc/machine-id`, `uninitialized`, a container with none, a system whose
  `/etc` is read-only and gets a new ID at every boot, or a container that
  bind-mounts the host's file.
- It is a false positive after an operating system reinstall over the same
  home, which is why removing `host_id` is the way out.

### A home directory shared across machines

A home directory that several Linux machines mount at once (NFS, say) breaks
what this program assumes: each machine would find the others' hooks,
`config.json`, local state and ownership records in the one data directory and
claim the same sessions under one machine ID; file locks (`flock`) are not
reliable between machines, so two collectors can corrupt local state; and
every machine's systemd user manager loads the same unit from
`~/.config/systemd/user`, so every machine runs the collector against the same
files.

So on Linux, `agent-archive setup` and `setup --refresh` refuse, before any
question and changing nothing, when the data directory or the systemd unit
directory is on a network filesystem. It looks at the filesystem type the
kernel reports (`/proc/self/mountinfo`) for each directory, or for the
nearest directory above it that exists, since setup has not made them yet. The
types it treats as network are `nfs`, `nfs4`, `cifs`, `smb3`, `smbfs`, `ceph`,
`glusterfs`, `afs`, `lustre`, `gpfs`, `beegfs`, `pvfs2` (OrangeFS), the
cluster filesystems `gfs2` and `ocfs2`, and the FUSE ones that name a remote
(`fuse.sshfs`, `fuse.rclone`, `fuse.s3fs`, `fuse.gcsfuse`, `fuse.glusterfs`,
`fuse.ceph-fuse`, `fuse.mfs` (MooseFS), `fuse.juicefs`). It does not treat as
network a local disk, `tmpfs`, an `overlay`, an encrypted view of local files
(`ecryptfs`, `fuse.gocryptfs`), a plain `fuse`, or `virtiofs` and `9p`, which
share a host's folders with one virtual machine and are what WSL mounts
Windows drives with. A home that autofs mounts on demand is looked at once it
is mounted (the check mounts it, as setup would). It is a guard, not proof: a
network filesystem under another name, or a system whose
`/proc/self/mountinfo` cannot be read, is not noticed. macOS is never checked,
and there `--allow-network-home` is accepted and does nothing, so one setup
command can serve both systems.

The refusal names the directory, the filesystem and why it matters, and gives
two ways out:

- Put the data directory on local disk: set `AGENT_ARCHIVE_HOME` (for the shell
  that runs setup, and so for its hooks and background job) to a path there,
  and give each machine a home of its own for the apps' hook files and
  the unit directory. `AGENT_ARCHIVE_HOME` does not move the unit directory,
  which is under the home directory, so a home that is shared stays refused
  because of it.
- If the home is only ever mounted on **one** machine (a home that is network
  storage for a single computer, or a diskless workstation), run
  `agent-archive setup --allow-network-home`. Setup then goes ahead, says on its
  checklist that it was allowed, and records `allow_network_home` in
  `config.json` (only while a directory is on a network filesystem), so
  `setup --refresh` and later runs do not refuse again. `status` keeps warning
  that the home is on a network filesystem, in its warnings and in `status
  --json`'s `warnings`, so the risk stays visible.

An installation made before the check, on a network home, is not changed:
`status` warns, and `setup --refresh` stops, changing nothing, until you run
`agent-archive setup --allow-network-home` (or move the data directory to local
disk).

If you opt in on a home that several machines do mount, expect this:

- The clone warning fires on every machine but the first, since only the
  first one's ID is recorded in the shared `config.json`.
- `agent-archive uninstall --delete-local-data` on any one of them deletes
  the shared data directory, and with it the state of every machine that
  uses it.
- Each machine runs its own background collector and hooks against the same
  directory, which agent-archive's ownership rules and file locks were not
  written for and which nothing here tests.

Silencing the warning by removing `host_id` does not make this safe.
