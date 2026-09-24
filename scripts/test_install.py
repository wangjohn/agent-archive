"""install.sh must install only a checksum-verified binary for this Mac."""
import hashlib
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest

INSTALL_SH = Path(__file__).resolve().parent.parent / 'install.sh'


def write_executable(path, body):
    path.write_text(body)
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


class InstallScriptTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.release = self.root / 'release'
        self.release.mkdir()
        for arch in ('arm64', 'amd64'):
            write_executable(self.release / f'agent-archive-darwin-{arch}',
                             f'#!/bin/sh\necho v9.9.9-{arch}\n')
        self.write_sums()
        self.home = self.root / 'home'
        self.home.mkdir()

    def write_sums(self, override=None):
        lines = []
        for arch in ('amd64', 'arm64'):
            name = f'agent-archive-darwin-{arch}'
            digest = hashlib.sha256((self.release / name).read_bytes()).hexdigest()
            if override and override.get(name) is not None:
                digest = override[name]
            if override and override.get(name) == '':
                continue
            lines.append(f'{digest}  {name}\n')
        (self.release / 'SHA256SUMS').write_text(''.join(lines))

    def run_install(self, system='Darwin', machine='arm64', rosetta='0', extra_env=None, path_dirs=(),
                    default_dir=False):
        shims = self.root / f'shims-{system}-{machine}-{rosetta}'
        shims.mkdir(exist_ok=True)
        write_executable(shims / 'uname',
                         f'#!/bin/sh\ncase "$1" in -s) echo {system} ;; -m) echo {machine} ;; esac\n')
        write_executable(shims / 'sysctl', f'#!/bin/sh\necho {rosetta}\n')
        env = {
            'HOME': str(self.home),
            'PATH': os.pathsep.join([str(shims), *map(str, path_dirs), '/usr/bin', '/bin']),
            'AGENT_ARCHIVE_DOWNLOAD_URL': self.release.as_uri(),
        }
        if not default_dir:
            # The default choice may be the real /usr/local/bin.
            env['AGENT_ARCHIVE_INSTALL_DIR'] = str(self.home / '.local' / 'bin')
        env.update(extra_env or {})
        return subprocess.run(['sh', str(INSTALL_SH)], env=env, capture_output=True, text=True)

    def test_installs_verified_binary_for_apple_silicon(self):
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        target = self.home / '.local' / 'bin' / 'agent-archive'
        self.assertEqual(subprocess.run([str(target)], capture_output=True, text=True).stdout, 'v9.9.9-arm64\n')
        self.assertIn('not on your PATH', result.stdout)
        self.assertEqual([p.name for p in target.parent.iterdir()], ['agent-archive'])

    @unittest.skipIf(os.access('/usr/local/bin', os.W_OK), '/usr/local/bin is writable here')
    def test_defaults_to_home_local_bin(self):
        result = self.run_install(default_dir=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.home / '.local' / 'bin' / 'agent-archive').exists())

    def test_picks_intel_build_and_native_build_under_rosetta(self):
        target = self.home / '.local' / 'bin' / 'agent-archive'
        result = self.run_install(machine='x86_64')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('v9.9.9-amd64', result.stdout)
        result = self.run_install(machine='x86_64', rosetta='1')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(subprocess.run([str(target)], capture_output=True, text=True).stdout, 'v9.9.9-arm64\n')

    def test_upgrades_existing_install_in_place(self):
        existing = self.root / 'existing-bin'
        existing.mkdir()
        write_executable(existing / 'agent-archive', '#!/bin/sh\necho v0.0.1\n')
        result = self.run_install(path_dirs=[existing], default_dir=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'to {existing}/agent-archive', result.stdout)
        self.assertIn('Next, run:  agent-archive setup', result.stdout)
        self.assertFalse((self.home / '.local').exists())

    def test_honors_install_dir_override(self):
        chosen = self.root / 'chosen'
        result = self.run_install(extra_env={'AGENT_ARCHIVE_INSTALL_DIR': str(chosen)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((chosen / 'agent-archive').exists())

    def test_rejects_checksum_mismatch(self):
        self.write_sums({'agent-archive-darwin-arm64': '0' * 64})
        result = self.run_install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('checksum mismatch', result.stderr)
        self.assertFalse((self.home / '.local' / 'bin' / 'agent-archive').exists())

    def test_rejects_missing_checksum_entry(self):
        self.write_sums({'agent-archive-darwin-arm64': ''})
        result = self.run_install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('no entry for agent-archive-darwin-arm64', result.stderr)

    def test_rejects_non_macos(self):
        result = self.run_install(system='Linux', machine='x86_64')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('macOS only', result.stderr)
        self.assertFalse((self.home / '.local').exists())


if __name__ == '__main__':
    unittest.main()
