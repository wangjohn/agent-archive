# Using more than one Mac

Several Macs can archive into the same bucket and prefix. Each Mac runs its
own setup, with its own machine ID, hooks, collector, and local state.

- **Ownership is per Mac.** A session belongs to the Mac that registered it.
  Only that Mac uploads it, republishes it, and deletes it when retention
  expires or `backfill undo` removes it. Local state is never shared or
  synced between Macs.
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
