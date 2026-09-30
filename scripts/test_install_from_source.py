"""Exercise the source installer without building Go code or touching this Mac."""

import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("install-from-source.sh")


def executable(path, contents):
    path.write_text(contents)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


class InstallFromSourceTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / "home"
        self.home.mkdir()
        self.shims = self.root / "shims"
        self.shims.mkdir()
        executable(self.shims / "uname", '#!/bin/sh\ncase "$1" in -s) echo Darwin ;; -m) echo x86_64 ;; esac\n')
        executable(self.shims / "sysctl", "#!/bin/sh\necho 0\n")
        executable(
            self.shims / "go",
            '#!/bin/sh\n'
            '[ "$1" = build ] || exit 2\n'
            'while [ "$1" != -o ]; do shift; done\n'
            'cat > "$2" <<\'EOF\'\n#!/bin/sh\necho dev-test-commit\nEOF\n'
            'chmod +x "$2"\n',
        )
        # The real security and codesign would read this Mac's Keychain.
        self.codesign_log = self.root / "codesign.log"
        self.identities("     0 valid identities found\n")
        executable(self.shims / "codesign",
                   f'#!/bin/sh\necho "$@" >> "{self.codesign_log}"\n')
        self.env = dict(os.environ, HOME=str(self.home),
                        PATH=f"{self.shims}:/usr/bin:/bin")

    def identities(self, listing):
        executable(self.shims / "security",
                   '#!/bin/sh\n'
                   '[ "$*" = "find-identity -v -p codesigning" ] || exit 2\n'
                   f"cat <<'EOF'\n{listing}EOF\n")

    def codesign_calls(self):
        if not self.codesign_log.exists():
            return []
        return self.codesign_log.read_text().splitlines()

    def run_script(self, *args, env=None):
        return subprocess.run(["bash", str(SCRIPT), *args], env=env or self.env,
                              capture_output=True, text=True)

    def test_default_install_is_separate_and_repeatable(self):
        target = self.home / ".local/share/agent-archive-dev/bin/agent-archive"
        for _ in range(2):
            result = self.run_script()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn(f"Installed dev-test-commit at {target}", result.stdout)
            self.assertEqual(subprocess.check_output([target], text=True), "dev-test-commit\n")

    def test_replace_current_uses_path_and_replaces_binary(self):
        current = self.root / "bin"
        current.mkdir()
        target = current / "agent-archive"
        executable(target, "#!/bin/sh\necho v0.1.1\n")
        env = dict(self.env, PATH=f"{self.shims}:{current}:/usr/bin:/bin")
        result = self.run_script("--replace-current", env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(subprocess.check_output([target], text=True), "dev-test-commit\n")
        self.assertFalse((self.home / ".local").exists())

    def test_build_failure_keeps_existing_binary(self):
        target = self.root / "bin" / "agent-archive"
        target.parent.mkdir()
        executable(target, "#!/bin/sh\necho old\n")
        executable(self.shims / "go", "#!/bin/sh\nexit 9\n")
        result = self.run_script("--destination", str(target))
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(subprocess.check_output([target], text=True), "old\n")

    def test_rejects_symlink_and_relative_destination(self):
        target = self.root / "bin" / "agent-archive"
        target.parent.mkdir()
        target.symlink_to(self.root / "other")
        result = self.run_script("--destination", str(target))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("symlink", result.stderr)
        result = self.run_script("--destination", "agent-archive")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("absolute path", result.stderr)

    def test_rejects_directory_destination(self):
        target = self.root / "bin" / "agent-archive"
        target.mkdir(parents=True)
        result = self.run_script("--destination", str(target))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not a regular file", result.stderr)

    def test_unsigned_without_identity_warns(self):
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("signed ad hoc", result.stderr)
        self.assertEqual(self.codesign_calls(), [])

    def test_signs_with_developer_id_and_release_identifier(self):
        self.identities(
            '  1) AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA "Apple Development: Someone (XYZ)"\n'
            '  2) BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB "Developer ID Application: Someone (ABC)"\n'
            '     2 valid identities found\n')
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("ad hoc", result.stderr)
        [call] = self.codesign_calls()
        self.assertIn("--sign BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", call)
        self.assertIn("--identifier agent-archive-darwin-amd64", call)

    def test_falls_back_to_any_valid_identity(self):
        self.identities('  1) AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA "Apple Development: Someone (XYZ)"\n'
                        '     1 valid identities found\n')
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        [call] = self.codesign_calls()
        self.assertIn("--sign AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", call)

    def test_identity_override_and_opt_out(self):
        self.identities('  1) BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB "Developer ID Application: Someone (ABC)"\n')
        result = self.run_script(env=dict(self.env, AGENT_ARCHIVE_SIGN_IDENTITY="My Cert"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--sign My Cert", self.codesign_calls()[0])
        result = self.run_script(env=dict(self.env, AGENT_ARCHIVE_SIGN_IDENTITY="-"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.codesign_calls()), 1)

    def test_signing_failure_keeps_existing_binary(self):
        target = self.root / "bin" / "agent-archive"
        target.parent.mkdir()
        executable(target, "#!/bin/sh\necho old\n")
        self.identities('  1) BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB "Developer ID Application: Someone (ABC)"\n')
        executable(self.shims / "codesign", "#!/bin/sh\nexit 1\n")
        result = self.run_script("--destination", str(target))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("AGENT_ARCHIVE_SIGN_IDENTITY=-", result.stderr)
        self.assertEqual(subprocess.check_output([target], text=True), "old\n")


if __name__ == "__main__":
    unittest.main()
