"""The release gate must reject each absent secret without exposing values."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parent
CHECK = SCRIPTS / 'check-release-signing.sh'
INSTALL_SH = SCRIPTS.parent / 'install.sh'
TEAM = 'SYNTH12345'


def with_team(script, team):
    """script with its team_id line naming team, whatever it named before."""
    return re.sub(r'^team_id="[A-Z0-9]*"$', f'team_id="{team}"', script, count=1, flags=re.M)


class ReleaseSigningTest(unittest.TestCase):
    def setUp(self):
        # A copy of install.sh that names the synthetic signing team, as the
        # real one must before the first release.
        self.root = Path(tempfile.mkdtemp())
        self.install_sh = self.root / 'install.sh'
        self.install_sh.write_text(with_team(INSTALL_SH.read_text(), TEAM))
        self.names = (
            'APPLE_CERTIFICATE_P12_BASE64 APPLE_CERTIFICATE_PASSWORD '
            'APPLE_SIGNING_IDENTITY APPLE_ID APPLE_TEAM_ID '
            'APPLE_APP_SPECIFIC_PASSWORD'
        ).split()
        self.env = dict(os.environ, APPLE_SIGNING_ENABLED='true', INSTALL_SH=str(self.install_sh),
                        **dict.fromkeys(self.names, 'synthetic-secret'))
        self.env['APPLE_TEAM_ID'] = TEAM

    def run_check(self, env):
        return subprocess.run(['bash', str(CHECK)], env=env, capture_output=True)

    def test_required_configuration(self):
        result = self.run_check(self.env)
        self.assertEqual(result.returncode, 0, result.stderr)
        for name in ['APPLE_SIGNING_ENABLED'] + self.names:
            with self.subTest(missing=name):
                missing = self.env.copy()
                missing.pop(name)
                result = self.run_check(missing)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn(b'synthetic-secret', result.stdout + result.stderr)

    def test_install_script_must_name_the_signing_team(self):
        cases = {
            'other team': with_team(self.install_sh.read_text(), 'OTHER12345'),
            'no team': with_team(self.install_sh.read_text(), ''),
            'set twice': self.install_sh.read_text() + f'\nteam_id="{TEAM}"\n',
        }
        for name, text in cases.items():
            with self.subTest(case=name):
                self.install_sh.write_text(text)
                result = self.run_check(self.env)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(b'team_id', result.stderr)
                self.assertNotIn(TEAM.encode(), result.stdout + result.stderr)
                self.assertNotIn(b'OTHER12345', result.stdout + result.stderr)

    def test_shipped_install_script_sets_team_id_once(self):
        lines = [line for line in INSTALL_SH.read_text().splitlines() if line.startswith('team_id=')]
        self.assertEqual(len(lines), 1, lines)
        self.assertRegex(lines[0], r'^team_id="[A-Z0-9]*"$')


if __name__ == '__main__':
    unittest.main()
