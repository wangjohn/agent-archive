"""Static and fake-Docker checks of scripts/acceptance/linux (the live Linux run).

The live run needs privileged Docker and is never part of CI. These tests do
not start anything: they check the scripts parse (and pass shellcheck when it
is installed), that --dry-run needs no Docker, that guest.sh refuses to run
anywhere but the machine host.sh makes, and, with a fake docker and a fake go
on PATH, that host.sh removes only what it made (names under its own
aa-accept-<random> prefix, never a container of the developer's) on success,
on failure and on interruption.
"""

import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import tempfile
import time
import unittest

DIR = Path(__file__).resolve().parent / "acceptance" / "linux"
HOST = DIR / "host.sh"
GUEST = DIR / "guest.sh"

FAKE_DOCKER = r"""#!/bin/sh
# Records every call; answers the few questions host.sh asks.
echo "$*" >> "$FAKE_LOG"
case "$1" in
  info) echo aarch64 ;;
  exec)
    case "$*" in
      *is-system-running*) echo running ;;
      *guest.sh*) [ -z "$FAKE_GUEST_SLEEP" ] || sleep "$FAKE_GUEST_SLEEP"; exit "${FAKE_GUEST_STATUS:-0}" ;;
      *os-release*) echo "Ubuntu 24.04 (fake), systemd 255 (fake)" ;;
    esac ;;
esac
exit 0
"""

FAKE_GO = r"""#!/bin/sh
echo "go $*" >> "$FAKE_LOG"
# `go build -o X` and `go test -c -o X` write X.
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then : > "$2"; fi
  shift
done
exit 0
"""


def executable(path, contents):
    path.write_text(contents)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


class AcceptanceScriptsTest(unittest.TestCase):
    def test_scripts_parse_and_are_executable(self):
        for script in (HOST, GUEST):
            self.assertTrue(os.access(script, os.X_OK), f"{script} is not executable")
            subprocess.run(["bash", "-n", str(script)], check=True)

    def test_shellcheck_is_clean_when_installed(self):
        shellcheck = shutil.which("shellcheck")
        if shellcheck is None:
            self.skipTest("shellcheck is not installed")
        # Warnings and errors only: shellcheck releases differ in which
        # info-level notes they print (0.9's SC2317 flags functions run only
        # from a trap as unreachable), and a note is not a defect.
        subprocess.run([shellcheck, "-x", "--severity=warning", str(HOST), str(GUEST)], check=True)

    def test_dry_run_needs_no_docker(self):
        with tempfile.TemporaryDirectory() as tools:
            # The few tools the script itself uses, and no docker or go.
            for tool in ("dirname", "od", "tr", "sed", "cat", "date"):
                os.symlink(shutil.which(tool), os.path.join(tools, tool))
            result = subprocess.run(
                ["/bin/bash", str(HOST), "--dry-run"],
                env={"PATH": tools, "HOME": tools},
                capture_output=True,
                text=True,
            )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("nothing is built, pulled or started", result.stdout)
        self.assertRegex(result.stdout, r"aa-accept-[0-9a-f]{8}-machine")

    def test_unknown_argument_is_refused(self):
        result = subprocess.run(["bash", str(HOST), "--nope"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)

    def test_guest_refuses_to_run_outside_the_machine(self):
        # No marker: refused before it can touch anything.
        env = {k: v for k, v in os.environ.items() if k != "AA_ACCEPT_GUEST"}
        result = subprocess.run(["bash", str(GUEST)], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("only inside the disposable machine", result.stderr)
        # A marker alone is not enough: this is not the machine (no /acceptance,
        # not root, or not Linux).
        env["AA_ACCEPT_GUEST"] = "aa-accept-test"
        result = subprocess.run(["bash", str(GUEST)], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("not the disposable acceptance machine", result.stderr)

    def test_guest_requires_the_marker_only_the_dockerfile_writes(self):
        marker = "/etc/agent-archive-acceptance-machine"
        self.assertIn(f"[ -f {marker} ]", GUEST.read_text())
        self.assertRegex((DIR / "Dockerfile").read_text(), rf"(?m)^RUN .*>{re.escape(marker)}$")
        self.assertNotIn(marker, HOST.read_text(), "host.sh must not make the marker: only the image has it")

    def test_guest_cache_home_is_private_even_with_a_permissive_umask(self):
        guest = GUEST.read_text()
        command = 'install -d -o ada -g ada -m 0700 "$XDG_CACHE"'
        self.assertIn(command, guest)
        self.assertLess(guest.index(command), guest.index("aa_xdg setup"))
        # Exercise the actual fixture command, substituting this test's own
        # account and temporary path so no host account or cache is touched.
        command = command.replace("-o ada -g ada", f"-o {os.getuid()} -g {os.getgid()}")
        for mask in ("0000", "0002", "0022"):
            for exists in (False, True):
                with self.subTest(umask=mask, exists=exists), tempfile.TemporaryDirectory() as root:
                    cache = Path(root) / "cache"
                    if exists:
                        cache.mkdir()
                        cache.chmod(0o777)
                    subprocess.run(
                        ["bash", "-ec", f'umask "$1"; XDG_CACHE=$2; {command}', "fixture", mask, str(cache)],
                        check=True,
                    )
                    info = cache.stat()
                    self.assertEqual(stat.S_IMODE(info.st_mode), 0o700)
                    self.assertEqual((info.st_uid, info.st_gid), (os.getuid(), os.getgid()))

    def test_status_checks_keep_session_and_import_counts_with_current_hook_wording(self):
        guest = GUEST.read_text()
        self.assertIn("'Claude Code.* hooks installed +1 session' aa_xdg status", guest)
        self.assertIn("'Cursor +hooks installed +no sessions yet .* 1 imported' aa_xdg status", guest)
        self.assertNotIn("hooks on", guest)

    def test_scripts_never_name_anyone_elses_container(self):
        for script in (HOST, GUEST, DIR / "Dockerfile", DIR / "README.md"):
            text = script.read_text()
            for line in text.splitlines():
                if "agent-archive-minio" in line:
                    self.assertRegex(
                        line,
                        r"(never|not|isn.t|without|your own)",
                        f"{script.name} mentions agent-archive-minio other than to say it is left alone: {line}",
                    )


class FakeDockerRunTest(unittest.TestCase):
    """host.sh against a recording docker: what it removes and when."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.bin = root / "bin"
        self.bin.mkdir()
        self.log = root / "calls.log"
        self.log.write_text("")
        executable(self.bin / "docker", FAKE_DOCKER)
        executable(self.bin / "go", FAKE_GO)
        self.env = {
            "PATH": f"{self.bin}:/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
            "HOME": str(root),
            "FAKE_LOG": str(self.log),
            "TMPDIR": str(root),
        }

    def calls(self):
        return self.log.read_text().splitlines()

    def removals(self):
        """The names every removal command was given."""
        names = []
        for call in self.calls():
            words = call.split()
            if words[:2] == ["rm", "-f"]:
                names += words[2:]
            elif words[:2] == ["network", "rm"] or words[:2] == ["volume", "rm"]:
                names += words[2:]
            elif words[:1] == ["rmi"]:
                names += words[1:]
        return names

    def check_cleanup(self):
        removed = self.removals()
        prefix = re.search(r"network create (aa-accept-[0-9a-f]{8})-net", "\n".join(self.calls()))
        self.assertIsNotNone(prefix, self.calls())
        self.assertEqual(
            sorted(removed),
            sorted(
                f"{prefix.group(1)}-{what}"
                for what in ("machine", "minio", "builder", "mc-bucket", "mc-list", "net", "image", "build")
            ),
        )
        self.assertNotIn("agent-archive-minio", " ".join(self.calls()))
        for call in self.calls():
            self.assertNotIn(" ps", f" {call}")  # it never lists a developer's containers

    def run_host(self, **extra):
        env = dict(self.env, **{k: str(v) for k, v in extra.items()})
        return subprocess.run(["bash", str(HOST)], env=env, capture_output=True, text=True, timeout=60)

    def test_success_removes_only_its_own_and_exits_zero(self):
        result = self.run_host()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.check_cleanup()

    def test_failed_checks_exit_with_their_status_and_still_clean_up(self):
        result = self.run_host(FAKE_GUEST_STATUS=3)
        self.assertEqual(result.returncode, 3, result.stdout + result.stderr)
        self.check_cleanup()

    def test_keep_leaves_everything(self):
        result = self.run_host(KEEP=1)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.removals(), [])
        self.assertIn("kept:", result.stdout)

    def test_interrupt_cleans_up(self):
        env = dict(self.env, FAKE_GUEST_SLEEP="20")
        output = Path(self.temp.name) / "host.out"
        # Its own process group, so the fake docker's sleep can be cleared away
        # at the end. Only the script itself is signalled: its cleanup must run
        # while the (fake) docker exec is still going.
        with output.open("w") as out:
            process = subprocess.Popen(["bash", str(HOST)], env=env, stdout=out, stderr=out, start_new_session=True)
            try:
                for _ in range(100):
                    if any("guest.sh" in call for call in self.calls()):
                        break
                    time.sleep(0.1)
                else:
                    self.fail("host.sh never reached guest.sh: " + "\n".join(self.calls()))
                process.send_signal(signal.SIGTERM)
                self.assertEqual(process.wait(timeout=10), 143)
            finally:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait()
        self.check_cleanup()


if __name__ == "__main__":
    unittest.main()
