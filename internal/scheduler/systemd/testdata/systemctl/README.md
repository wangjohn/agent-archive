# systemctl fixtures

These are the outputs of `systemctl --version` (`version.txt`) and of
`systemctl --user show <ref>.timer <ref>.service
--property=Id,LoadState,ActiveState,SubState,FragmentPath,DropInPaths`
(`show-<state>.txt`), the questions the systemd adapter asks, that its tests
answer from.

**They are captured from real user managers**, on September 30 2026, in
disposable privileged containers (booted with `/sbin/init`, the user lingering,
`systemctl --user` run as that user with its `XDG_RUNTIME_DIR` and user bus):

| Directory | System | `systemctl --version` |
| --- | --- | --- |
| `239` | Rocky Linux 8 (RHEL 8's systemd, with `StandardOutput=append:` backported) | `systemd 239 (239-82.el8_10.19)` |
| `245` | Ubuntu 20.04 | `systemd 245 (245.4-4ubuntu3.24)` |
| `252` | Debian 12 | `systemd 252 (252.39-1~deb12u2)` |
| `255` | Ubuntu 24.04 | `systemd 255 (255.4-1ubuntu8.17)` |

The job was the adapter's own: its `Plan` for a stand-in collector, written to
`~/.config/systemd/user`, then driven through each state with `systemctl
--user`. The user's unit files and job name were then replaced with `@TIMER@`,
`@SERVICE@` and `@REF@`, and, in `show-another.txt`, the user's unit directory
with `@OTHER@` (another installation's directory); nothing else was edited.

The states: `missing` (no unit files), `inactive` (files written, never
enabled), `running` (a run in progress: the service `activating`), `loaded`
(the timer waiting between runs), `another` (the loaded job, from another
directory), `dropin` (a drop-in in `<ref>.service.d`), `failed-service` (the
last run exited non-zero; the timer is still active) and `masked` (the timer
masked by a `/dev/null` link in the higher-priority `~/.config/systemd/user.control`,
since `systemctl --user mask` refuses a unit whose file is in
`~/.config/systemd/user`; 239 reports `FragmentPath=/dev/null`, later versions
the link). `255/show-typewide-dropin.txt` is the loaded job under a drop-in for
every user service (`/usr/lib/systemd/user/service.d/10-timeout-abort.conf`,
which Fedora ships).

In a container `OnBootSec=` counts from the manager's start, so the
`running` run was started with `systemctl --user start --no-block` rather than
waited for; the timer is `running` while a run it started is in progress
(239, 245) and `waiting` beside one started by hand (252, 255).

What the captures showed that the documentation had not said: every
asked-for property is printed even when empty (`FragmentPath=` for a unit not
found); a masked unit that was active is `failed`; and (seen on 255) unit
files written after the manager started are found without a `daemon-reload`.

To capture again, boot a disposable systemd container (never a workstation),
enable lingering for a test user, write the adapter's units, and run the
command above in each state.
