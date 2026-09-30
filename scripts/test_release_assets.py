"""The four release binaries must be checksummed and published consistently.

Two things the release workflow cannot show without a tag: that SHA256SUMS is
written the same way with sha256sum (Linux) or shasum (macOS), and that every
place the workflow names the released files names the same ones.
"""
import hashlib
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parent
ROOT = SCRIPTS.parent
WRITE_CHECKSUMS = SCRIPTS / 'write-checksums.sh'
RELEASE_YML = ROOT / '.github' / 'workflows' / 'release.yml'
BASH = shutil.which('bash')

BINARIES = [
    'agent-archive-darwin-amd64',
    'agent-archive-darwin-arm64',
    'agent-archive-linux-amd64',
    'agent-archive-linux-arm64',
]


def make_dist(directory, names=BINARIES):
    directory.mkdir(parents=True, exist_ok=True)
    for name in names:
        (directory / name).write_bytes(f'synthetic {name}\n'.encode())


def make_shim(directory, name, digest, skip_args=0):
    """A stand-in checksum tool printing digest for each file it is given."""
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / name
    path.write_text(f'#!/bin/sh\nshift {skip_args}\nfor f in "$@"; do echo "{digest}  $f"; done\n')
    path.chmod(path.stat().st_mode | stat.S_IEXEC)


def run_write_checksums(dist, path=None):
    """Runs write-checksums.sh; a given path replaces PATH entirely."""
    env = {'PATH': path} if path is not None else None
    return subprocess.run([BASH, str(WRITE_CHECKSUMS), str(dist)], env=env, capture_output=True, text=True)


class WriteChecksumsTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root, True)
        self.dist = self.root / 'dist'
        make_dist(self.dist)

    def sums(self):
        return (self.dist / 'SHA256SUMS').read_text()

    def test_lists_all_four_in_order_in_the_format_install_sh_reads(self):
        result = run_write_checksums(self.dist)
        self.assertEqual(result.returncode, 0, result.stderr)
        expected = ''.join(
            f'{hashlib.sha256((self.dist / name).read_bytes()).hexdigest()}  {name}\n' for name in BINARIES
        )
        self.assertEqual(self.sums(), expected)

    def test_prefers_sha256sum_and_falls_back_to_shasum(self):
        shims = self.root / 'shims'
        make_shim(shims, 'shasum', 'b' * 64, skip_args=2)  # shasum -a 256 FILE...
        result = run_write_checksums(self.dist, path=str(shims))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.sums(), ''.join(f'{"b" * 64}  {name}\n' for name in BINARIES))

        make_shim(shims, 'sha256sum', 'a' * 64)
        result = run_write_checksums(self.dist, path=str(shims))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.sums(), ''.join(f'{"a" * 64}  {name}\n' for name in BINARIES))

    def test_fails_without_a_checksum_tool(self):
        empty = self.root / 'empty'
        empty.mkdir()
        result = run_write_checksums(self.dist, path=str(empty))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('sha256sum or shasum', result.stderr)
        self.assertFalse((self.dist / 'SHA256SUMS').exists())

    def test_fails_when_a_binary_is_missing(self):
        for name in BINARIES:
            with self.subTest(missing=name):
                dist = self.root / f'without-{name}'
                make_dist(dist, [other for other in BINARIES if other != name])
                result = run_write_checksums(dist)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(name, result.stderr)
                self.assertFalse((dist / 'SHA256SUMS').exists())

    def test_names_the_same_binaries_this_test_expects(self):
        script = WRITE_CHECKSUMS.read_text()
        listed = re.search(r'assets=\(\n(.*?)\n\)', script, re.S).group(1).split()
        self.assertEqual(listed, BINARIES)


def job_text(workflow, job):
    """The text of one top-level job in the workflow."""
    match = re.search(rf'^  {re.escape(job)}:\n(.*?)(?=^  [\w-]+:\n|\Z)', workflow, re.S | re.M)
    if not match:
        raise AssertionError(f'no job {job!r} in the workflow')
    return match.group(1)


def block_lines(text, header):
    """The indented lines of the YAML block scalar that follows header."""
    lines = text.splitlines()
    for index, line in enumerate(lines):
        if line.strip() == header:
            indent = len(line) - len(line.lstrip())
            body = []
            for following in lines[index + 1:]:
                if following.strip() and len(following) - len(following.lstrip()) <= indent:
                    break
                if following.strip():
                    body.append(following.strip())
            return body
    raise AssertionError(f'no {header!r} in the text')


class ReleaseWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.workflow = RELEASE_YML.read_text()
        self.publish = job_text(self.workflow, 'publish')
        self.dist = {f'dist/{name}' for name in BINARIES}

    def step(self, name):
        match = re.search(rf'^      - name: {re.escape(name)}\n(.*?)(?=^      - |\Z)', self.publish, re.S | re.M)
        self.assertIsNotNone(match, f'no step {name!r} in publish')
        return match.group(1)

    def test_attestation_subjects_are_exactly_the_released_binaries(self):
        attest = self.publish[self.publish.index('actions/attest-build-provenance@'):]
        subjects = block_lines(attest, 'subject-path: |')
        self.assertEqual(len(subjects), len(set(subjects)), subjects)
        self.assertEqual(set(subjects), self.dist)

    def test_uploaded_and_released_assets_are_the_binaries_and_checksums(self):
        upload = self.publish[self.publish.index('name: agent-archive-${{ github.ref_name }}'):]
        uploaded = block_lines(upload, 'path: |')
        self.assertEqual(len(uploaded), len(set(uploaded)), uploaded)
        self.assertEqual(set(uploaded), self.dist | {'dist/SHA256SUMS'})

        create = self.publish[self.publish.index('gh release create "$VERSION"'):]
        released = re.findall(r'^\s+(dist/\S+?)(?: \\)?$', create, re.M)
        self.assertEqual(len(released), len(set(released)), released)
        self.assertEqual(set(released), self.dist | {'dist/SHA256SUMS'})

    def test_build_jobs_together_produce_the_unsigned_binaries(self):
        produced = []
        for job in ('build', 'build-linux'):
            produced += block_lines(job_text(self.workflow, job), 'path: |')
        self.assertEqual(sorted(produced), sorted(self.dist))

    def test_only_the_macos_binaries_are_signed_and_notarized(self):
        for name in ('Codesign', 'Notarize'):
            with self.subTest(step=name):
                text = self.step(name)
                self.assertIn('darwin', text)
                self.assertNotIn('linux', text)

    def test_publish_waits_for_both_builds_and_downloads_both(self):
        self.assertRegex(self.publish, r'needs: \[build, build-linux\]')
        self.assertRegex(self.publish, r'name: unsigned\n\s+path: dist')
        self.assertRegex(self.publish, r'name: linux\n\s+path: dist')

    def test_every_build_job_keeps_the_tag_and_main_guard(self):
        for job in ('build', 'build-linux'):
            with self.subTest(job=job):
                text = job_text(self.workflow, job)
                self.assertIn(r'^v[0-9]+\.[0-9]+\.[0-9]+$', text)
                self.assertIn('git merge-base --is-ancestor "$GITHUB_SHA" origin/main', text)
                self.assertIn('VERSION: ${{ github.ref_name }}', text)

    def test_the_workflow_still_runs_only_on_release_tags(self):
        head = self.workflow[: self.workflow.index('jobs:')]
        self.assertRegex(head, r"on:\n  push:\n    tags:\n      - 'v\*\.\*\.\*'\n")
        self.assertNotIn('workflow_dispatch', head)
        self.assertIn('environment: release', self.publish)


if __name__ == '__main__':
    unittest.main()
