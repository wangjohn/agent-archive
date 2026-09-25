"""The bucket purge recipes in the docs must delete only what they say.

The shell blocks marked <!-- purge-recipe:... --> in docs/security/privacy.md
and docs/getting-started/uninstall.md are run as written, in
bash and in zsh (macOS's default shell), against a fake `aws` that serves a
bucket from a temporary directory. They must delete superseded sources and
orphaned ones, keep every current source and metadata object and everything
outside the session prefix, refuse to delete anything when a metadata object
can't be read, and remove whole only the sessions older than the filter
version named, or captured by the machine named.
"""
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import unittest

DOCS = Path(__file__).resolve().parent.parent / 'docs'
RECIPE_PAGES = [DOCS / 'security' / 'privacy.md', DOCS / 'getting-started' / 'uninstall.md']

# A stand-in for the few AWS CLI calls the recipe makes, over $FAKE_S3/<bucket>/<key>.
# It drains its stdin first, as a CLI may: a recipe loop that doesn't keep
# aws off the list it is reading would then skip entries (and, in the
# listing, take a skipped session's current source for unreferenced).
FAKE_AWS = r'''#!/bin/sh
cat > /dev/null
root="$FAKE_S3"
object() { b=${1#s3://}; printf '%s/%s' "$root" "$b"; }
case "$1 $2" in
  "s3api list-objects-v2")
    shift 2
    while [ $# -gt 0 ]; do
      case "$1" in
        --bucket) bucket=$2; shift 2 ;;
        --prefix) prefix=$2; shift 2 ;;
        --query|--output) shift 2 ;;
        *) echo "fake aws: unexpected $1" >&2; exit 2 ;;
      esac
    done
    keys=$(cd "$root/$bucket" && find . -type f ! -name '*.unreadable' | sed 's|^\./||' |
      awk -v p="$prefix" 'index($0, p) == 1' | sort | paste -s -d '\t' -)
    if [ -z "$keys" ]; then echo None; else printf '%s\n' "$keys"; fi
    ;;
  "s3 cp")
    [ "$4" = - ] || exit 2
    f=$(object "$3")
    [ -f "$f" ] && [ ! -f "$f.unreadable" ] || { echo "fake aws: cannot read $3" >&2; exit 1; }
    cat "$f"
    ;;
  "s3 rm")
    f=$(object "$3")
    echo "$3" >> "$FAKE_S3_LOG"
    if [ "${4:-}" = --recursive ]; then rm -rf "$f"; else rm -f "$f"; fi
    ;;
  *) echo "fake aws: unexpected $*" >&2; exit 2 ;;
esac
'''

FAKE_AGENT_ARCHIVE = '#!/bin/sh\necho "$1" >> "$FAKE_AGENT_ARCHIVE_LOG"\n'


def recipe_blocks():
    blocks = {}
    for page in RECIPE_PAGES:
        for match in re.finditer(r'<!-- purge-recipe:([a-z-]+)[^>]*-->\n```sh\n(.*?)```', page.read_text(), re.S):
            blocks[match.group(1)] = match.group(2)
    return blocks


def write_executable(path, body):
    path.write_text(body)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


class PurgeRecipeTest(unittest.TestCase):
    shells = [s for s in ('bash', 'zsh') if shutil.which(s)]

    def setUp(self):
        if not shutil.which('jq'):
            self.skipTest('jq is not installed')
        self.blocks = recipe_blocks()
        self.assertEqual(sorted(self.blocks), ['delete', 'list', 'machine', 'old-sessions'])
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root)

    def build(self, prefix, sessions, extra=(), unreadable=()):
        """sessions: {session dir: (current source or None, [other sources], filter version[, machine])}."""
        bucket = self.root / 's3' / 'my-archive-bucket'
        if bucket.exists():
            shutil.rmtree(bucket)
        for directory, (current, others, version, *machine) in sessions.items():
            base = bucket / prefix / 'sessions' / directory
            base.mkdir(parents=True)
            for name in ([current] if current else []) + others:
                (base / name).write_bytes(b'gz')
            if current:
                metadata = {'filter_version': str(version), 'machine_id': (machine or ['m-other'])[0],
                            'source_bundle': {'key': f'sessions/{directory}/{current}'}}
                (base / 'metadata.json').write_text(json.dumps(metadata))
        for key in extra:
            path = bucket / key
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('x')
        for key in unreadable:
            (bucket / (key + '.unreadable')).write_text('')
        return bucket

    def run_recipe(self, shell, prefix, names):
        bin_dir = self.root / 'bin'
        bin_dir.mkdir(exist_ok=True)
        write_executable(bin_dir / 'aws', FAKE_AWS)
        write_executable(bin_dir / 'agent-archive', FAKE_AGENT_ARCHIVE)
        work = self.root / 'work'
        if work.exists():
            shutil.rmtree(work)
        work.mkdir()
        script = '\n'.join(self.blocks[name] for name in names)
        script = script.replace('prefix=agent-archive/ ', f'prefix={prefix} ', 1)
        script = script.replace('machine=0123456789abcdef0123456789abcdef', 'machine=m-retired', 1)
        env = dict(os.environ,
                   PATH=f'{bin_dir}{os.pathsep}{os.environ["PATH"]}',
                   FAKE_S3=str(self.root / 's3'),
                   FAKE_S3_LOG=str(self.root / 'rm.log'),
                   FAKE_AGENT_ARCHIVE_LOG=str(self.root / 'agent-archive.log'))
        return subprocess.run([shell, '-c', script], cwd=work, env=env, stdin=subprocess.DEVNULL,
                              capture_output=True, text=True)

    @staticmethod
    def keys(bucket):
        return sorted(str(p.relative_to(bucket)) for p in bucket.rglob('*') if p.is_file() and not p.name.endswith('.unreadable'))

    def sessions(self):
        return {
            'claude/aaaa': ('source.a2.jsonl.gz', ['source.a1.jsonl.gz', 'source.a0.jsonl.gz'], 9),
            'codex/bbbb': ('source.b1.jsonl.gz', [], 10),
            'cursor/cccc': (None, ['source.c1.jsonl.gz'], 0),  # orphan: no metadata
        }

    def test_deletes_only_unreferenced_sources(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', 'nested/archive/', ''):
                with self.subTest(shell=shell, prefix=prefix):
                    p = prefix
                    bucket = self.build(p, self.sessions(), extra=[f'{p}.setup-test/x.json', 'other/sessions/claude/aaaa/source.z.jsonl.gz', f'{p}sessions/claude/aaaa/notes.txt'])
                    result = self.run_recipe(shell, prefix, ['list', 'delete'])
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.keys(bucket), sorted([
                        f'{p}.setup-test/x.json',
                        'other/sessions/claude/aaaa/source.z.jsonl.gz',
                        f'{p}sessions/claude/aaaa/metadata.json',
                        f'{p}sessions/claude/aaaa/notes.txt',
                        f'{p}sessions/claude/aaaa/source.a2.jsonl.gz',
                        f'{p}sessions/codex/bbbb/metadata.json',
                        f'{p}sessions/codex/bbbb/source.b1.jsonl.gz',
                    ]))
                    self.assertEqual((self.root / 'agent-archive.log').read_text().split()[-1], 'pause')

    def test_unreadable_metadata_deletes_nothing(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                bucket = self.build('agent-archive/', self.sessions(),
                                    unreadable=['agent-archive/sessions/codex/bbbb/metadata.json'])
                before = self.keys(bucket)
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'])
                self.assertIn('nothing deleted', result.stderr)
                self.assertEqual(self.keys(bucket), before)
                self.assertIn('agent-archive/sessions/codex/bbbb/metadata.json',
                              (self.root / 'work' / 'failed.txt').read_text())

    def test_old_sessions_are_removed_whole(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                bucket = self.build('agent-archive/', self.sessions(),
                                    unreadable=['agent-archive/sessions/codex/bbbb/metadata.json'])
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'old-sessions'])
                self.assertEqual(result.returncode, 0, result.stderr)
                remaining = self.keys(bucket)
                # Filter 9 goes whole; an unreadable metadata is never taken for old.
                self.assertFalse([k for k in remaining if '/claude/aaaa/' in k], remaining)
                self.assertIn('agent-archive/sessions/codex/bbbb/metadata.json', remaining)
                removed = (self.root / 'rm.log').read_text().split()
                self.assertEqual(removed[0], 's3://my-archive-bucket/agent-archive/sessions/claude/aaaa/metadata.json')

    def test_one_machines_sessions_are_removed_whole(self):
        sessions = {
            'claude/aaaa': ('source.a2.jsonl.gz', ['source.a1.jsonl.gz'], 10, 'm-retired'),
            'codex/bbbb': ('source.b1.jsonl.gz', [], 10, 'm-current'),
            'codex/dddd': ('source.d1.jsonl.gz', [], 10, 'm-retired'),
            'cursor/eeee': ('source.e1.jsonl.gz', [], 10, 'm-retired'),
        }
        for shell in self.shells:
            for prefix in ('agent-archive/', ''):
                with self.subTest(shell=shell, prefix=prefix):
                    bucket = self.build(prefix, sessions,
                                        unreadable=[f'{prefix}sessions/cursor/eeee/metadata.json'])
                    result = self.run_recipe(shell, prefix, ['machine'])
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.keys(bucket), sorted([
                        f'{prefix}sessions/codex/bbbb/metadata.json',
                        f'{prefix}sessions/codex/bbbb/source.b1.jsonl.gz',
                        f'{prefix}sessions/cursor/eeee/metadata.json',
                        f'{prefix}sessions/cursor/eeee/source.e1.jsonl.gz',
                    ]))


if __name__ == '__main__':
    unittest.main()
