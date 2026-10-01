"""The real-systemd CI job keeps the name and the pinned image it is required by.

Branch protection lists real-systemd as a required check, and matches it by the
check's name: renaming the job (or giving it a name: or a matrix, which change
the check's name) leaves the rule waiting on a check that never reports. The
job is validated on one runner image (dev/contributing/testing.md); on
ubuntu-latest a new image would change systemd under a required check without
a change in this repository. Read without PyYAML, like test_release_assets.py.
"""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parent.parent
TEST_YML = ROOT / '.github' / 'workflows' / 'test.yml'
TESTING_MD = ROOT / 'dev' / 'contributing' / 'testing.md'

JOB = 'real-systemd'
PINNED_UBUNTU = re.compile(r'ubuntu-\d\d\.\d\d')


def jobs(workflow):
    """Each top-level job's id mapped to its text, in the order they appear."""
    start = re.search(r'(?m)^jobs:\n', workflow)
    if not start:
        raise AssertionError('no jobs: in the workflow')
    body = workflow[start.end():]
    end = re.search(r'(?m)^[^\s#]', body)  # the next top-level key; comments are not one
    if end:
        body = body[:end.start()]
    found = {}
    for match in re.finditer(r'(?ms)^  ([\w-]+):[ \t]*(?:#[^\n]*)?\n(.*?)(?=^  [\w-]+:|\Z)', body):
        found[match.group(1)] = match.group(2)
    return found


def job_key(text, key):
    """The value of one of the job's own keys (four spaces in), or None."""
    match = re.search(rf'(?m)^    {re.escape(key)}:(.*)$', text)
    if not match:
        return None
    return re.sub(r'\s+#.*$', '', match.group(1)).strip().strip('\'"')


class RealSystemdJobTest(unittest.TestCase):
    def setUp(self):
        self.jobs = jobs(TEST_YML.read_text())

    def job(self):
        self.assertIn(JOB, self.jobs, f'test.yml has no job named {JOB}; branch protection requires it by name')
        return self.jobs[JOB]

    def test_the_job_is_named_exactly_real_systemd(self):
        text = self.job()
        # A name: or a matrix would change the check's name from the job id.
        self.assertIsNone(job_key(text, 'name'), f'{JOB} must not set name:, which renames the required check')
        self.assertIsNone(job_key(text, 'strategy'), f'{JOB} must not have a matrix, which renames the required check')

    def test_runs_on_a_pinned_ubuntu_image(self):
        runs_on = job_key(self.job(), 'runs-on')
        self.assertIsNotNone(runs_on, f'{JOB} has no runs-on')
        self.assertRegex(
            runs_on,
            rf'^{PINNED_UBUNTU.pattern}$',
            f'{JOB} must run on a pinned image such as ubuntu-24.04, not {runs_on!r}; bump it deliberately',
        )

    def test_the_testing_docs_name_the_same_image(self):
        runs_on = job_key(self.job(), 'runs-on')
        self.assertIn(f'`{runs_on}`', TESTING_MD.read_text(), f'dev/contributing/testing.md should name {runs_on}')


class JobsParserTest(unittest.TestCase):
    def test_finds_each_job_and_its_own_keys(self):
        workflow = (
            'name: Test\n'
            'jobs:\n'
            '  a:\n'
            '    runs-on: ubuntu-latest\n'
            '  real-systemd:  # comment\n'
            '    # runs-on: ubuntu-latest\n'
            '    runs-on: "ubuntu-24.04"  # pinned\n'
            '    steps:\n'
            '      - name: step\n'
            'other: x\n'
        )
        found = jobs(workflow)
        self.assertEqual(list(found), ['a', 'real-systemd'])
        self.assertEqual(job_key(found['real-systemd'], 'runs-on'), 'ubuntu-24.04')
        self.assertIsNone(job_key(found['real-systemd'], 'name'))


if __name__ == '__main__':
    unittest.main()
