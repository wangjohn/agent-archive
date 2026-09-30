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


VERIFY = SCRIPTS / 'verify-linux-release.sh'
VERSION = 'v0.0.0-test'


class VerifyLinuxReleaseTest(unittest.TestCase):
    """The static-linking check must reject anything but a static ELF file."""

    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root, True)
        self.dist = self.root / 'dist'
        self.shims = self.root / 'shims'
        self.shims.mkdir()

    def fake_readelf(self, header_ok, program_headers):
        """A readelf that answers -h with header_ok and -lW with program_headers."""
        path = self.shims / 'readelf'
        path.write_text(
            '#!/bin/sh\n'
            'if [ "$1" = -h ]; then\n'
            + ('  exit 0\n' if header_ok else '  echo "readelf: Error: Not an ELF file" >&2; exit 1\n')
            + 'fi\n'
            + ('' if header_ok else 'echo "readelf: Error: Not an ELF file" >&2; exit 1\n')
            + f'echo "{program_headers}"\n'
        )
        path.chmod(path.stat().st_mode | stat.S_IEXEC)

    def run_verify(self, contents):
        """Runs the script on files that embed the version and hold contents."""
        self.dist.mkdir(exist_ok=True)
        for name in ('agent-archive-linux-amd64', 'agent-archive-linux-arm64'):
            (self.dist / name).write_bytes(contents + b'\n' + VERSION.encode() + b'\n')
        env = {'PATH': f'{self.shims}:/usr/bin:/bin', 'VERSION': VERSION}
        return subprocess.run([BASH, str(VERIFY), str(self.dist)], env=env, capture_output=True, text=True)

    def test_rejects_a_file_that_is_not_elf_when_readelf_exists(self):
        # readelf prints nothing on stdout for these, which looks like "no
        # INTERP header" and so like a static binary unless checked first.
        self.fake_readelf(header_ok=False, program_headers='')
        for kind, contents in {
            'Mach-O': b'\xcf\xfa\xed\xfe' + bytes(64),
            'script': b'#!/bin/sh\necho hi\n',
        }.items():
            with self.subTest(kind=kind):
                result = self.run_verify(contents)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn('not statically linked', result.stderr)

    def test_rejects_a_dynamically_linked_elf(self):
        self.fake_readelf(header_ok=True, program_headers='  INTERP  0x0 0x0 0x0 0x1c 0x1c R 0x1')
        result = self.run_verify(b'\x7fELF')
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn('not statically linked', result.stderr)

    @unittest.skipUnless(shutil.which('readelf'), 'needs readelf')
    def test_rejects_a_text_file_with_the_real_readelf(self):
        env = {'PATH': '/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin', 'VERSION': VERSION}
        self.dist.mkdir()
        for name in ('agent-archive-linux-amd64', 'agent-archive-linux-arm64'):
            (self.dist / name).write_text(f'not an ELF file\n{VERSION}\n')
        result = subprocess.run([BASH, str(VERIFY), str(self.dist)], env=env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn('not statically linked', result.stderr)


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


def strip_comment(line):
    return re.sub(r'\s+#.*$', '', line).rstrip()


def scalar(text):
    text = text.strip()
    if len(text) >= 2 and text[0] == text[-1] and text[0] in '\'"':
        return text[1:-1]
    return {} if text == '{}' else text


def parse_block(lines):
    """A nested dict from the mappings and lists of a small YAML block.

    Enough for the workflow's `on:` and `permissions:` blocks (PyYAML is not
    guaranteed to be installed): `key: value`, `key:` with nested keys or
    `- item` lines, and comments.
    """
    root = {}
    # Each entry: the indent of a key, the mapping holding it, and the key.
    stack = [(-1, {None: root}, None)]
    for raw in lines:
        line = strip_comment(raw)
        if not line.strip() or line.strip().startswith('#'):
            continue
        indent = len(line) - len(line.lstrip())
        text = line.strip()
        while stack[-1][0] >= indent:
            stack.pop()
        _, holder, holder_key = stack[-1]
        container = holder[holder_key]
        if text.startswith('- '):
            if container == {}:
                container = holder[holder_key] = []
            container.append(scalar(text[2:]))
            continue
        key, _, value = text.partition(':')
        container[key] = scalar(value) if value.strip() else {}
        stack.append((indent, container, key))
    return root


def top_level_block(workflow, key):
    """A top-level key's inline value and the lines nested under it."""
    lines = workflow.splitlines()
    for index, line in enumerate(lines):
        if line.startswith(f'{key}:'):
            body = []
            for following in lines[index + 1:]:
                if following.strip() and not following.startswith((' ', '#')):
                    break
                body.append(following)
            return strip_comment(line)[len(key) + 1:].strip(), body
    raise AssertionError(f'no top-level {key!r} in the workflow')


def job_permissions(job):
    """The job's own `permissions:` mapping."""
    lines = job.splitlines()
    for index, line in enumerate(lines):
        if line == '    permissions:':
            body = []
            for following in lines[index + 1:]:
                if following.strip() and len(following) - len(following.lstrip()) <= 4:
                    break
                body.append(following)
            return parse_block(body)
    raise AssertionError('job has no permissions block')


def steps(job):
    """The text of each step of a job, in order."""
    parts = re.split(r'^      - ', job, flags=re.M)
    return ['      - ' + part for part in parts[1:]]


def run_texts(workflow):
    """The text of every `run:` in the workflow."""
    lines = workflow.splitlines()
    texts = []
    for index, line in enumerate(lines):
        match = re.match(r'^(\s*)(?:- )?run:(.*)$', line)
        if not match:
            continue
        rest = match.group(2).strip()
        if rest in ('|', '>'):
            indent = len(match.group(1))
            body = []
            for following in lines[index + 1:]:
                if following.strip() and len(following) - len(following.lstrip()) <= indent:
                    break
                body.append(following)
            texts.append('\n'.join(body))
        else:
            texts.append(rest)
    return texts


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
        _, body = top_level_block(self.workflow, 'on')
        self.assertEqual(parse_block(body), {'push': {'tags': ['v*.*.*']}})

    def test_workflow_and_job_permissions_are_least_privilege(self):
        inline, _ = top_level_block(self.workflow, 'permissions')
        self.assertEqual(inline, '{}')
        read = {'contents': 'read'}
        self.assertEqual(job_permissions(job_text(self.workflow, 'build')), read)
        self.assertEqual(job_permissions(job_text(self.workflow, 'build-linux')), read)
        self.assertEqual(
            job_permissions(self.publish),
            {'contents': 'write', 'id-token': 'write', 'attestations': 'write'},
        )

    def test_publish_is_a_release_environment_job_with_no_condition(self):
        self.assertRegex(self.publish, r'(?m)^    environment: release$')
        self.assertNotRegex(self.publish, r'(?m)^    if:')
        # The one step that runs after a failure only removes the keychain.
        for text in steps(self.publish):
            if 'always()' in text:
                self.assertIn('name: Clean up signing keychain', text)

    def test_checksums_are_written_after_signing_and_before_attestation(self):
        texts = steps(self.publish)

        def index(marker):
            found = [i for i, text in enumerate(texts) if marker in text]
            self.assertEqual(len(found), 1, f'{marker!r} should appear in exactly one publish step')
            return found[0]

        checksums = index('name: Recompute checksums after signing')
        self.assertIn('./scripts/write-checksums.sh dist', texts[checksums])
        self.assertNotIn('shasum', texts[checksums])
        self.assertGreater(checksums, index('name: Codesign'))
        self.assertGreater(checksums, index('name: Notarize'))
        self.assertLess(checksums, index('actions/attest-build-provenance@'))

    def test_every_action_is_pinned_to_a_full_commit(self):
        used = re.findall(r'(?m)^\s*(?:- )?uses: (\S+)', self.workflow)
        self.assertGreaterEqual(len(used), 10)
        for action in used:
            with self.subTest(action=action):
                self.assertRegex(action, r'@[0-9a-f]{40}$')

    def test_checkouts_keep_no_credentials_and_go_has_no_module_cache(self):
        for job in ('build', 'build-linux', 'publish'):
            with self.subTest(job=job):
                text = job_text(self.workflow, job)
                self.assertEqual(text.count('actions/checkout@'), 1)
                self.assertEqual(text.count('persist-credentials: false'), 1)
                self.assertNotIn('persist-credentials: true', text)
        for job in ('build', 'build-linux'):
            with self.subTest(job=job):
                text = job_text(self.workflow, job)
                self.assertIn('cache: false', text)
                self.assertNotIn('cache: true', text)

    def test_build_jobs_see_no_secrets(self):
        for job in ('build', 'build-linux'):
            with self.subTest(job=job):
                self.assertNotIn('secrets.', job_text(self.workflow, job))

    def test_artifacts_hand_off_under_matching_names_into_dist(self):
        def uploads(job):
            return re.findall(r'upload-artifact@\S+[^\n]*\n\s+with:\n\s+name: (\S+)', job_text(self.workflow, job))

        self.assertEqual(uploads('build'), ['unsigned'])
        self.assertEqual(uploads('build-linux'), ['linux'])
        downloads = re.findall(
            r'download-artifact@\S+[^\n]*\n\s+with:\n\s+name: (\S+)\n\s+path: (\S+)\n', self.publish
        )
        self.assertEqual(sorted(downloads), [('linux', 'dist'), ('unsigned', 'dist')])

    def test_each_platform_builds_on_its_own_runner_with_its_own_checks(self):
        linux = job_text(self.workflow, 'build-linux')
        self.assertRegex(linux, r'(?m)^    runs-on: ubuntu-24\.04$')
        self.assertRegex(linux, r'(?m)^        run: \./scripts/build-release\.sh linux$')
        self.assertNotIn('build-release.sh darwin', linux)
        self.assertRegex(linux, r'(?m)^        run: \./scripts/verify-linux-release\.sh$')
        darwin = job_text(self.workflow, 'build')
        self.assertRegex(darwin, r'(?m)^    runs-on: macos-14$')
        self.assertRegex(darwin, r'(?m)^        run: \./scripts/build-release\.sh darwin$')
        self.assertNotIn('build-release.sh linux', darwin)

    def test_release_is_created_for_an_existing_tag(self):
        create = self.publish[self.publish.index('gh release create "$VERSION"'):]
        self.assertIn('--verify-tag', create.split('\n')[0])

    def test_no_expression_is_expanded_into_script_text(self):
        texts = run_texts(self.workflow)
        self.assertGreaterEqual(len(texts), 8)
        for text in texts:
            self.assertNotIn('${{', text)


if __name__ == '__main__':
    unittest.main()
