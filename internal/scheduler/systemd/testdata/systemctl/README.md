# systemctl fixtures

These are the outputs of `systemctl --version` (`version.txt`) and of
`systemctl --user show <ref>.timer <ref>.service` (`show-<state>.txt`) that the
systemd adapter's tests answer from, for systemd 239 (RHEL 8), 245 (Ubuntu
20.04) and 252 (Debian 12).

**They are hand-built, not captured from live systems.** They are written from
systemd's documented property formats (`systemctl show`, `org.freedesktop.systemd1`
unit properties): the unit blocks separated by an empty line, `Key=Value`
lines, properties with an empty value not printed, and the values `LoadState`,
`ActiveState` and `SubState` take for each state. 239 and 252 hold what the
adapter asks for (`--property=Id,LoadState,ActiveState,SubState,FragmentPath,DropInPaths`),
and 245 holds a longer, unfiltered-style dump, so the reader is tested on both
shapes. Whether a real manager prints exactly this is what the real-manager CI
job of the systemd work (5b-2) checks; until then a difference between these
and a live system is a bug in the fixtures, and the fix is to replace them with
a recording.

`@REF@`, `@TIMER@`, `@SERVICE@` and `@OTHER@` stand for the job's name, its two
unit files at the site under test, and another user's unit directory.

The states: `loaded` (timer active and waiting), `running` (service
activating), `missing` (both units not found), `inactive` (files present,
timer inactive), `another` (loaded from another user's files), `masked`,
`failed-service` (the last run failed, the timer is active) and `dropin` (a
drop-in overrides the service).
