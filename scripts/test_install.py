"""install.sh must install only a checksum- and signature-verified binary for this Mac."""
import hashlib
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import unittest

INSTALL_SH = Path(__file__).resolve().parent.parent / 'install.sh'
TEAM = 'SYNTH12345'


def with_team(script, team):
    """script with its team_id line naming team, whatever it named before."""
    return re.sub(r'^team_id="[A-Z0-9]*"$', f'team_id="{team}"', script, count=1, flags=re.M)
DEVELOPER_ID_REQUIREMENT = (
    'anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists '
    'and certificate leaf[field.1.2.840.113635.100.6.1.13] exists '
    f'and certificate leaf[subject.OU] = "{TEAM}"'
)

# A codesign stand-in. The fake binaries are "signed" by $FAKE_SIGNING_TEAM
# (empty: unsigned). It records its arguments and passes only a strict
# verification against exactly the Developer ID requirement for that team.
CODESIGN_SHIM = """#!/bin/sh
printf '%s\\n' "$@" >> "$CODESIGN_LOG"
[ "$1" = --verify ] && [ "$2" = --strict ] || exit 1
[ -n "$FAKE_SIGNING_TEAM" ] || exit 1
want="-R=anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = \\"$FAKE_SIGNING_TEAM\\""
[ "$3" = "$want" ] || exit 3
[ -f "$4" ] || exit 1
"""


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
        # The shipped script names no team until the first release; the
        # tests run a copy that names the synthetic one.
        self.install_sh = self.root / 'install.sh'
        self.install_sh.write_text(with_team(INSTALL_SH.read_text(), TEAM))
        self.codesign_log = self.root / 'codesign.log'

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
                    default_dir=False, script=None, signing_team=TEAM):
        shims = self.root / f'shims-{system}-{machine}-{rosetta}'
        shims.mkdir(exist_ok=True)
        write_executable(shims / 'uname',
                         f'#!/bin/sh\ncase "$1" in -s) echo {system} ;; -m) echo {machine} ;; esac\n')
        write_executable(shims / 'sysctl', f'#!/bin/sh\necho {rosetta}\n')
        write_executable(shims / 'codesign', CODESIGN_SHIM)
        env = {
            'HOME': str(self.home),
            'PATH': os.pathsep.join([str(shims), *map(str, path_dirs), '/usr/bin', '/bin']),
            'AGENT_ARCHIVE_DOWNLOAD_URL': self.release.as_uri(),
            'CODESIGN_LOG': str(self.codesign_log),
            'FAKE_SIGNING_TEAM': signing_team,
        }
        if not default_dir:
            # The default choice may be the real /usr/local/bin.
            env['AGENT_ARCHIVE_INSTALL_DIR'] = str(self.home / '.local' / 'bin')
        env.update(extra_env or {})
        return subprocess.run(['sh', str(script or self.install_sh)], env=env, capture_output=True, text=True)

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

    def test_verifies_the_developer_id_signature_with_the_pinned_team(self):
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'Signature verified (Developer ID, team {TEAM})', result.stdout)
        args = self.codesign_log.read_text().splitlines()
        self.assertEqual(args[:3], ['--verify', '--strict', f'-R={DEVELOPER_ID_REQUIREMENT}'])
        self.assertTrue(args[3].endswith('agent-archive-darwin-arm64'), args)

    def test_rejects_a_binary_signed_by_another_team(self):
        result = self.run_install(signing_team='OTHER12345')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('not signed by the agent-archive Developer ID', result.stderr)
        self.assertFalse((self.home / '.local' / 'bin' / 'agent-archive').exists())

    def test_rejects_an_unsigned_binary(self):
        result = self.run_install(signing_team='')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('not signed by the agent-archive Developer ID', result.stderr)
        self.assertFalse((self.home / '.local').exists())

    def test_shipped_script_without_a_team_installs_nothing(self):
        if 'team_id=""' not in INSTALL_SH.read_text():
            self.skipTest('install.sh names its signing team')
        # It says so before downloading: pointed at a release that doesn't
        # exist, it still fails on the missing team, not on the download.
        result = self.run_install(script=INSTALL_SH,
                                  extra_env={'AGENT_ARCHIVE_DOWNLOAD_URL': (self.root / 'no-release').as_uri()})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('names no signing team yet', result.stderr)
        self.assertFalse((self.home / '.local').exists())
        self.assertFalse(self.codesign_log.exists())


if __name__ == '__main__':
    unittest.main()
