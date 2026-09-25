# Using more than one Mac

Several Macs can archive into the same bucket and prefix. Each Mac runs its
own setup, with its own machine ID, hooks, collector, and local state.

- **Ownership is per Mac.** A session belongs to the Mac that registered it.
  Only that Mac uploads it, republishes it, and deletes it when retention
  expires or `backfill undo` removes it. Local state is never shared or
  synced between Macs. So the sessions of a Mac you retire or wipe stay in
  the bucket until you delete them; see
  [delete the archive](../getting-started/uninstall.md#delete-the-archive-in-the-bucket),
  which also shows a lifecycle rule that cleans up after a Mac that is gone.
- **Reading is shared.** `list`, `show`, and `handoff` read the bucket, so
  they see every Mac's sessions. Each session's metadata records the
  `machine_id` that captured it.
- **Handoff across Macs.** `agent-archive handoff --latest` on another Mac
  downloads the session from the archive. It matches the project only when
  the repository is checked out at the same path; otherwise use a session ID
  from `list`. Push your branch first: uncommitted changes stay on the Mac
  that made them. See [handoff](handoff.md).
- **Retention is per Mac too.** Each Mac's retention setting applies to the
  sessions it owns. Set the same value on every Mac if you want one policy.
- **Credentials.** Each Mac needs its own access to the bucket: an AWS
  profile, or R2 credentials entered in its own setup (stored in that Mac's
  Keychain). A key limited to one prefix (see
  [bucket permissions](../security/bucket-permissions.md)) works for several
  Macs sharing that prefix.

Don't copy the data directory from one Mac to another: two Macs that believe
they own the same sessions would publish over each other.

## Migration Assistant and Time Machine

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
