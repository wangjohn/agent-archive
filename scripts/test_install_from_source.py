"""Exercise the source installer without building Go code or touching this machine."""

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
        self.build_log = self.root / "build.log"
        self.sysctl_log = self.root / "sysctl.log"
        self.set_platform("Darwin", "x86_64")
        executable(
            self.shims / "go",
            '#!/bin/sh\n'
            '[ "$1" = build ] || exit 2\n'
            'echo "GOOS=$GOOS GOARCH=$GOARCH CGO_ENABLED=$CGO_ENABLED" >> "$BUILD_LOG"\n'
            'while [ "$1" != -o ]; do shift; done\n'
            'cat > "$2" <<\'EOF\'\n#!/bin/sh\necho dev-test-commit\nEOF\n'
            'chmod +x "$2"\n',
        )
        self.env = dict(os.environ, HOME=str(self.home), BUILD_LOG=str(self.build_log),
                        SYSCTL_LOG=str(self.sysctl_log), PATH=f"{self.shims}:/usr/bin:/bin")

    def set_platform(self, system, machine, rosetta="0"):
        executable(self.shims / "uname", f'#!/bin/sh\ncase "$1" in -s) echo {system} ;; -m) echo {machine} ;; esac\n')
        executable(self.shims / "sysctl", f'#!/bin/sh\necho sysctl >> "$SYSCTL_LOG"\necho {rosetta}\n')

    def built_for(self):
        return self.build_log.read_text().splitlines()

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

    def test_macos_builds_with_cgo_for_the_native_architecture(self):
        for machine, rosetta, arch in (("x86_64", "0", "amd64"), ("x86_64", "1", "arm64"), ("arm64", "0", "arm64")):
            with self.subTest(machine=machine, rosetta=rosetta):
                self.build_log.unlink(missing_ok=True)
                self.set_platform("Darwin", machine, rosetta)
                result = self.run_script()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"for darwin/{arch}...", result.stdout)
                self.assertEqual(self.built_for(), [f"GOOS=darwin GOARCH={arch} CGO_ENABLED=1"])

    def test_linux_builds_static_without_cgo(self):
        for machine, arch in (("x86_64", "amd64"), ("amd64", "amd64"), ("aarch64", "arm64"), ("arm64", "arm64")):
            with self.subTest(machine=machine):
                self.build_log.unlink(missing_ok=True)
                self.sysctl_log.unlink(missing_ok=True)
                self.set_platform("Linux", machine, rosetta="1")
                target = self.home / ".local/share/agent-archive-dev/bin/agent-archive"
                result = self.run_script()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"for linux/{arch}...", result.stdout)
                self.assertIn(f"Installed dev-test-commit at {target}", result.stdout)
                self.assertEqual(self.built_for(), [f"GOOS=linux GOARCH={arch} CGO_ENABLED=0"])
                self.assertFalse(self.sysctl_log.exists())
                self.assertEqual(subprocess.check_output([target], text=True), "dev-test-commit\n")

    def test_rejects_unsupported_architectures(self):
        for system, machine in (("Linux", "armv7l"), ("Linux", "riscv64"), ("Linux", "i686"), ("Darwin", "ppc64")):
            with self.subTest(system=system, machine=machine):
                self.set_platform(system, machine)
                result = self.run_script()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("unsupported", result.stderr)
                self.assertFalse(self.build_log.exists())
                self.assertFalse((self.home / ".local").exists())

    def test_rejects_unsupported_operating_systems(self):
        for system in ("FreeBSD", "MINGW64_NT-10.0", "SunOS"):
            with self.subTest(system=system):
                self.set_platform(system, "x86_64")
                result = self.run_script()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("macOS or Linux is required", result.stderr)
                self.assertFalse(self.build_log.exists())
                self.assertFalse((self.home / ".local").exists())

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


if __name__ == "__main__":
    unittest.main()
