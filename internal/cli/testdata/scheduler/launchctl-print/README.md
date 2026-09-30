# Recorded launchctl print output

What `launchctl print gui/UID/LABEL` says for the states the CLI tells apart,
read by `TestStatusBackgroundForEveryJobState` and
`TestParseJobStateOverRecordedOutput`. `@PLIST@` stands for the plist launchd
loaded the job from; the test replaces it with the plist it checks.

| File | launchctl exit | The CLI reads |
| --- | --- | --- |
| `running.txt` | 0 | `running` (`state = running`, `path =` is the plist) |
| `loaded-not-running.txt` | 0 | `loaded` (`state = not running`) |
| `missing.txt` | 113 | `missing` (`Could not find service`) |
| `another-installation.txt` | 0 | `another_installation` (`path =` is another plist) |
| `no-path-line.txt` | 0 | `unknown` (no `path =` line, only `stdout path =` and `stderr path =`) |
| `failed-unrelated.txt` | 1 | `unknown` (a failure that does not say the service is missing) |

These were written by hand in the shape macOS 14 and 15 print (the field
names and layout of a loaded LaunchAgent, including the `stdout path =` and
`stderr path =` lines that a parser looking for `path =` must not confuse with
the job's own), not captured from a live launchd: the tests never run the real
launchctl, and this repository does not ask a contributor to. If launchd's
output ever changes, add a file recorded from the new system beside these and
keep these: releases in the field still meet the old shape.
