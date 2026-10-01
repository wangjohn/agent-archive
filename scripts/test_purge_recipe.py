"""Execute every documented bucket-purge block against a disposable fake bucket."""
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

FAKE_AWS = r'''#!/bin/sh
cat > /dev/null
root=$FAKE_S3
bump() {
  counter="$FAKE_S3_STATE.$1"
  n=0; [ ! -f "$counter" ] || n=$(cat "$counter")
  n=$((n + 1)); printf '%s\n' "$n" > "$counter"
  COUNT=$n
}
object() { printf '%s/%s' "$root" "${1#s3://}"; }
case "$1 $2" in
  "s3api list-objects-v2")
    bump list
    [ "$COUNT" != "${FAKE_LIST_FAIL_AT:-0}" ] || { echo 'fake listing failure' >&2; exit 1; }
    shift 2
    while [ $# -gt 0 ]; do
      case "$1" in
        --bucket) bucket=$2; shift 2 ;;
        --prefix) prefix=$2; shift 2 ;;
        --query|--output) shift 2 ;;
        *) echo "unexpected aws option $1" >&2; exit 2 ;;
      esac
    done
    if [ "$COUNT" = "${FAKE_LIST_BAD_AT:-0}" ]; then
      echo '{"broken":true}'
    else
      (cd "$root/$bucket" && find . -type f ! -name '*.unreadable' | sed 's|^./||' |
        awk -v p="$prefix" 'index($0,p)==1' | LC_ALL=C sort) |
        jq -R -s 'split("\n")[:-1] | if length == 0 then null else . end'
    fi
    ;;
  "s3 cp")
    bump cp
    f=$(object "$3")
    [ "$COUNT" != "${FAKE_CP_FAIL_AT:-0}" ] && [ -f "$f" ] && [ ! -f "$f.unreadable" ] ||
      { echo "fake metadata read failure: $3" >&2; exit 1; }
    cat "$f"
    ;;
  "s3 rm")
    bump rm
    f=$(object "$3")
    printf '%s\n' "${3#s3://}" >> "$FAKE_S3_ATTEMPTS"
    [ "$COUNT" != "${FAKE_RM_FAIL_AT:-0}" ] || { echo "fake deletion failure: $3" >&2; exit 1; }
    rm -f "$f" || exit 1
    if [ "$COUNT" = "${FAKE_RM_CRASH_AT:-0}" ]; then
      kill -KILL "$PPID"
      exit 99
    fi
    printf '%s\n' "${3#s3://}" >> "$FAKE_S3_REMOVED"
    ;;
  *) echo "unexpected aws call: $*" >&2; exit 2 ;;
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
        self.assertEqual(sorted(self.blocks), ['all', 'delete', 'list', 'machine', 'old-sessions'])
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root)

    def build(self, prefix='agent-archive/', sessions=None, extra=(), unreadable=()):
        sessions = self.sessions() if sessions is None else sessions
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

    def run_recipe(self, shell, prefix, names, between=None, env_extra=None, tail=None):
        for name in ('state.json', 'attempts.log', 'removed.log', 'agent-archive.log'):
            (self.root / name).unlink(missing_ok=True)
        for path in self.root.glob('state.json.*'):
            path.unlink()
        bin_dir = self.root / 'bin'
        bin_dir.mkdir(exist_ok=True)
        write_executable(bin_dir / 'aws', FAKE_AWS)
        write_executable(bin_dir / 'agent-archive', FAKE_AGENT_ARCHIVE)
        marker_rm = bin_dir / 'rm'
        if (env_extra or {}).get('FAKE_MARKER_RM_FAIL'):
            write_executable(marker_rm, '#!/bin/sh\ncase "$1" in */VALID) exit 1 ;; esac\nexec /bin/rm "$@"\n')
        else:
            marker_rm.unlink(missing_ok=True)
        work = self.root / 'work'
        if work.exists():
            shutil.rmtree(work)
        work.mkdir()
        script = '\n'.join(self.blocks[name] for name in names)
        if tail:
            script += '\n' + tail
        if between:
            script = script.replace('purge_apply\n', between + '\npurge_apply\n', 1)
        script = script.replace('prefix=agent-archive/ ', f'prefix={prefix} ')
        script = script.replace('machine=0123456789abcdef0123456789abcdef', 'machine=m-retired')
        helper = work / 'helpers.sh'
        helper.write_text(self.blocks['list'].rsplit('purge_prepare unreferenced', 1)[0])
        env = dict(os.environ,
                   PATH=f'{bin_dir}{os.pathsep}{os.environ["PATH"]}',
                   FAKE_HELPERS=str(helper), FAKE_SHELL=shutil.which(shell),
                   AWS_CONFIG_FILE=str(self.root / 'aws-config'),
                   AWS_SHARED_CREDENTIALS_FILE=str(self.root / 'no-credentials'),
                   FAKE_S3=str(self.root / 's3'),
                   FAKE_S3_STATE=str(self.root / 'state.json'),
                   FAKE_S3_ATTEMPTS=str(self.root / 'attempts.log'),
                   FAKE_S3_REMOVED=str(self.root / 'removed.log'),
                   FAKE_AGENT_ARCHIVE_LOG=str(self.root / 'agent-archive.log'))
        env.update(env_extra or {})
        return subprocess.run([shell, '-c', script], cwd=work, env=env, stdin=subprocess.DEVNULL,
                              capture_output=True, text=True)

    @staticmethod
    def keys(bucket):
        return sorted(str(p.relative_to(bucket)) for p in bucket.rglob('*') if p.is_file() and not p.name.endswith('.unreadable'))

    def attempts(self):
        path = self.root / 'attempts.log'
        return path.read_text().splitlines() if path.exists() else []

    def removed(self):
        path = self.root / 'removed.log'
        return path.read_text().splitlines() if path.exists() else []

    def sessions(self):
        return {
            'claude/aaaa': ('source.a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2.jsonl.gz', ['source.a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1.jsonl.gz', 'source.a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0.jsonl.gz'], 9, 'm-retired'),
            'codex/bbbb': ('source.b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1.jsonl.gz', [], 10, 'm-current'),
            'cursor/cccc': (None, ['source.c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1.jsonl.gz'], 0),
            'cursor/dddd': ('source.d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1.jsonl.gz', [], 10, 'm-retired'),
        }

    def test_unreferenced_sources_keep_current_and_scope(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', 'nested/archive/', ''):
                with self.subTest(shell=shell, prefix=prefix):
                    bucket = self.build(prefix, extra=[f'{prefix}.setup-test/x.json',
                        'other/sessions/claude/aaaa/source.zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.jsonl.gz', f'{prefix}sessions/claude/aaaa/notes.txt'])
                    result = self.run_recipe(shell, prefix, ['list', 'delete'])
                    self.assertEqual(result.returncode, 0, result.stderr)
                    remaining = self.keys(bucket)
                    for key in remaining:
                        self.assertNotIn(key, [f'{prefix}sessions/claude/aaaa/source.a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0.jsonl.gz',
                                               f'{prefix}sessions/claude/aaaa/source.a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1.jsonl.gz',
                                               f'{prefix}sessions/cursor/cccc/source.c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1.jsonl.gz'])
                    self.assertIn(f'{prefix}sessions/claude/aaaa/source.a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2.jsonl.gz', remaining)
                    self.assertIn('other/sessions/claude/aaaa/source.zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.jsonl.gz', remaining)
                    self.assertTrue(all(k.startswith(f'my-archive-bucket/{prefix}sessions/') for k in self.removed()))

    def test_preflight_failures_delete_nothing(self):
        for shell in self.shells:
            for mode, names in [('source', ['list', 'delete']), ('old', ['list', 'old-sessions', 'delete']),
                                ('machine', ['list', 'machine', 'delete']), ('all', ['list', 'all', 'delete'])]:
                for failure in ('listing', 'bad-listing', 'unreadable', 'malformed',
                                'empty-metadata', 'multi-metadata', 'missing-source'):
                    with self.subTest(shell=shell, mode=mode, failure=failure):
                        bucket = self.build()
                        later = bucket / 'agent-archive/sessions/cursor/dddd/metadata.json'
                        env = {}
                        if failure == 'listing':
                            env['FAKE_LIST_FAIL_AT'] = '2' if mode != 'source' else '1'
                        elif failure == 'bad-listing':
                            env['FAKE_LIST_BAD_AT'] = '2' if mode != 'source' else '1'
                        elif failure == 'unreadable':
                            Path(str(later) + '.unreadable').write_text('')
                        elif failure == 'malformed':
                            later.write_text('{broken')
                        elif failure == 'empty-metadata':
                            later.write_text('')
                        elif failure == 'multi-metadata':
                            later.write_text(later.read_text() + '\n' + later.read_text())
                        else:
                            metadata = json.loads(later.read_text())
                            del metadata['source_bundle']['key']
                            later.write_text(json.dumps(metadata))
                        before = self.keys(bucket)
                        result = self.run_recipe(shell, 'agent-archive/', names, env_extra=env)
                        self.assertNotEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(self.keys(bucket), before)
                        self.assertEqual(self.attempts(), [], result.stderr)

    def test_stale_and_changed_plans_delete_nothing(self):
        for shell in self.shells:
            for intervention in ('rm "$purge_dir/VALID"',
                                 'echo 1 > "$purge_dir/created"',
                                 'echo changed >> "$FAKE_S3/my-archive-bucket/agent-archive/sessions/codex/bbbb/metadata.json"',
                                 'touch "$FAKE_S3/my-archive-bucket/agent-archive/sessions/codex/bbbb/new.txt"',
                                 'bucket=other-bucket', 'mode=all'):
                with self.subTest(shell=shell, intervention=intervention):
                    bucket = self.build()
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'], between=intervention)
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.attempts(), [], result.stderr)
                    if 'echo changed' not in intervention and 'touch' not in intervention:
                        self.assertEqual(self.keys(bucket), before)

    def test_invalid_current_source_identity_deletes_nothing(self):
        for shell in self.shells:
            for variant in ('nested', 'sibling', 'nonexistent'):
                with self.subTest(shell=shell, variant=variant):
                    bucket = self.build()
                    metadata_path = bucket / 'agent-archive/sessions/cursor/dddd/metadata.json'
                    metadata = json.loads(metadata_path.read_text())
                    original = metadata['source_bundle']['key']
                    name = original.rsplit('/', 1)[1]
                    metadata['source_bundle']['key'] = {
                        'nested': f'sessions/cursor/dddd/other/{name}',
                        'sibling': f'sessions/cursor/elsewhere/{name}',
                        'nonexistent': 'sessions/cursor/dddd/source.' + 'e' * 64 + '.jsonl.gz',
                    }[variant]
                    metadata_path.write_text(json.dumps(metadata))
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'])
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.keys(bucket), before)
                    self.assertEqual(self.attempts(), [])

    def test_marker_removal_failure_deletes_nothing(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                bucket = self.build()
                before = self.keys(bucket)
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'],
                                         env_extra={'FAKE_MARKER_RM_FAIL': '1'})
                self.assertNotEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.keys(bucket), before)
                self.assertEqual(self.attempts(), [])

    def test_partial_delete_reports_progress_and_stops(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                self.build()
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'],
                                         env_extra={'FAKE_RM_FAIL_AT': '2'})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(len(self.removed()), 1)
                self.assertEqual(len(self.attempts()), 2)
                self.assertIn('Already removed:', result.stderr)
                self.assertIn('Not confirmed removed', result.stderr)
                self.assertIn(self.removed()[0].removeprefix('my-archive-bucket/'), result.stderr)
                self.assertIn(self.attempts()[1].removeprefix('my-archive-bucket/'), result.stderr)

    def test_old_machine_and_all_exact_keys(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', 'nested/archive/', ''):
                for mode, names in [('old', ['list', 'old-sessions', 'delete']),
                                    ('machine', ['list', 'machine', 'delete']),
                                    ('all', ['list', 'all', 'delete'])]:
                    with self.subTest(shell=shell, prefix=prefix, mode=mode):
                        bucket = self.build(prefix, extra=[f'{prefix}.setup-test/x', 'outside/keep'])
                        result = self.run_recipe(shell, prefix, names)
                        self.assertEqual(result.returncode, 0, result.stderr)
                        remaining = self.keys(bucket)
                        if mode != 'all' or prefix:
                            self.assertIn('outside/keep', remaining)
                        if mode == 'old':
                            self.assertFalse(any(f'{prefix}sessions/claude/aaaa/' in k for k in remaining))
                            self.assertIn(f'{prefix}sessions/codex/bbbb/source.b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1.jsonl.gz', remaining)
                        elif mode == 'machine':
                            self.assertFalse(any(f'{prefix}sessions/claude/aaaa/' in k or
                                                 f'{prefix}sessions/cursor/dddd/' in k for k in remaining))
                            self.assertIn(f'{prefix}sessions/codex/bbbb/source.b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1.jsonl.gz', remaining)
                        else:
                            self.assertFalse(any(k.startswith(prefix) for k in remaining))

    def test_resume_every_deletion_boundary_in_fresh_shell(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', 'nested/archive/', ''):
                for mode, prepare, count in [('unreferenced', 'unreferenced', 3),
                                             ('machine', 'machine "$machine"', 6),
                                             ('old', 'old 10', 4), ('all', 'all', 9)]:
                    for boundary in range(1, count + (1 if mode == 'all' and not prefix else 0) + 1):
                        with self.subTest(shell=shell, prefix=prefix, mode=mode, boundary=boundary):
                            bucket = self.build(prefix, extra=['outside/keep'])
                            before = self.keys(bucket)
                            contents = {k: (bucket / k).read_bytes() for k in before}
                            tail = r'''
original=$purge_dir
purge_apply && exit 91
unset FAKE_RM_FAIL_AT
"$FAKE_SHELL" -c '. "$FAKE_HELPERS"; bucket=my-archive-bucket; prefix=$2; purge_resume "$1" && purge_apply' recovery "$original" "$prefix"
'''
                            names = ['list'] if mode == 'unreferenced' else ['list', {'machine':'machine', 'old':'old-sessions', 'all':'all'}[mode]]
                            result = self.run_recipe(shell, prefix, names,
                                env_extra={'FAKE_RM_FAIL_AT': str(boundary)}, tail=tail)
                            self.assertEqual(result.returncode, 0, result.stderr)
                            remaining = self.keys(bucket)
                            if mode == 'unreferenced':
                                expected = [k for k in before if not (k.endswith('.jsonl.gz') and
                                    ('/cursor/cccc/' in k or '/claude/aaaa/source.a0' in k or '/claude/aaaa/source.a1' in k))]
                            elif mode == 'all':
                                expected = [k for k in before if not k.startswith(prefix)]
                            else:
                                dirs = ['claude/aaaa'] + (['cursor/dddd'] if mode == 'machine' else [])
                                expected = [k for k in before if not any(k.startswith(f'{prefix}sessions/{d}/') for d in dirs)]
                            self.assertEqual(remaining, expected)
                            self.assertEqual({k: (bucket / k).read_bytes() for k in remaining},
                                             {k: contents[k] for k in expected})

    def test_expired_and_single_use_attempts_need_reviewed_resume(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                self.build()
                tail = r"""
original=$purge_dir
# Advance the clock rather than modify the sealed attempt.
expiry=$(($(cat "$purge_dir/created") + 301))
date() { printf '%s\n' "$expiry"; }
purge_apply && exit 91
purge_apply && exit 92
purge_resume "$original" || exit 93
purge_apply || exit 94
purge_apply && exit 95
exit 0
"""
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'], tail=tail)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(len(self.attempts()), 6)

    def test_progress_failure_and_repeated_recovery(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                bucket = self.build(extra=['outside/keep'])
                before = self.keys(bucket)
                tail = r"""
mv() { case "$2" in */pending) return 1 ;; esac; command mv "$@"; }
purge_apply && exit 91
unset -f mv
original=$purge_dir
purge_resume "$original" || exit 92
# A second interruption must preserve the original exact scope.
export FAKE_RM_FAIL_AT=3
purge_apply && exit 93
unset FAKE_RM_FAIL_AT
original=$purge_dir
purge_resume "$original" && purge_apply
"""
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'], tail=tail)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.keys(bucket), [k for k in before if not any(
                    k.startswith('agent-archive/sessions/' + d + '/') for d in ('claude/aaaa', 'cursor/dddd'))])
            for blocked in ('inflight', 'removed', 'errors', 'pending'):
                with self.subTest(shell=shell, blocked=blocked):
                    bucket = self.build()
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine', 'delete'],
                        between='mkdir "$purge_dir/' + blocked + '"')
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(self.attempts(), [])
                    self.assertEqual(self.keys(bucket), before)

    def test_crash_after_remote_delete_before_progress(self):
        for shell in self.shells:
            for boundary in (1, 2, 5, 6):
                with self.subTest(shell=shell, boundary=boundary):
                    bucket = self.build(extra=['outside/keep'])
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine', 'delete'],
                        env_extra={'FAKE_RM_CRASH_AT': str(boundary)})
                    self.assertNotEqual(result.returncode, 0)
                    original = re.findall(r'Plan (.*?): [0-9]+ exact keys', result.stdout)[-1]
                    tail = '. "$FAKE_HELPERS"; bucket=my-archive-bucket; prefix=agent-archive/; purge_resume "' + original + '" && purge_apply'
                    resumed = self.run_recipe(shell, 'agent-archive/', [], tail=tail)
                    self.assertEqual(resumed.returncode, 0, resumed.stderr)
                    self.assertEqual(self.keys(bucket), [k for k in before if not any(
                        k.startswith('agent-archive/sessions/' + d + '/') for d in ('claude/aaaa', 'cursor/dddd'))])

    def test_resume_conflicts_delete_nothing(self):
        for shell in self.shells:
            for intervention in (
                'chmod 600 "$original/manifest.json"; echo broken > "$original/manifest.json"',
                'bucket=wrong', 'AWS_ENDPOINT_URL=https://wrong.example',
                'touch "$FAKE_S3/my-archive-bucket/agent-archive/sessions/claude/aaaa/new.txt"',
                'echo changed >> "$FAKE_S3/my-archive-bucket/agent-archive/sessions/cursor/dddd/metadata.json"',
                'cp "$original/meta.1" "$FAKE_S3/my-archive-bucket/agent-archive/sessions/claude/aaaa/metadata.json"',
                'export FAKE_LIST_BAD_AT=4', 'export FAKE_LIST_FAIL_AT=4'):
                with self.subTest(shell=shell, intervention=intervention):
                    self.build()
                    tail = r'''
original=$purge_dir
purge_apply && exit 91
unset FAKE_RM_FAIL_AT
''' + intervention + r'''
purge_resume "$original" && exit 92
'''
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'],
                        env_extra={'FAKE_RM_FAIL_AT':'2'}, tail=tail)
                    self.assertEqual(result.returncode, 1, result.stderr)
                    self.assertEqual(len(self.attempts()), 2, result.stderr)

    def test_resume_rejects_corrupt_progress_and_reappeared_metadata(self):
        for shell in self.shells:
            for intervention in (
                ': > "$original/removed"',
                'echo bogus >> "$original/removed"',
                'rm "$original/removed"',
                'rm "$original/progress.json"',
                'chmod 600 "$original/progress.json"; echo broken > "$original/progress.json"',
                'echo bogus >> "$original/tombstones"'):
                with self.subTest(shell=shell, intervention=intervention):
                    self.build()
                    tail = r'''
original=$purge_dir
purge_apply && exit 91
unset FAKE_RM_FAIL_AT
cp "$original/meta.1" "$FAKE_S3/my-archive-bucket/agent-archive/sessions/claude/aaaa/metadata.json"
''' + intervention + r'''
purge_resume "$original" && purge_apply && exit 92
exit 0
'''
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'],
                        env_extra={'FAKE_RM_FAIL_AT':'2'}, tail=tail)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(len(self.attempts()), 2, result.stderr)
                    self.assertIn('progress', result.stderr)

    def test_crash_around_progress_capsule_rename_recovers(self):
        for shell in self.shells:
            for boundary in (2, 3):
                for phase in ('before', 'after'):
                    with self.subTest(shell=shell, boundary=boundary, phase=phase):
                        bucket = self.build()
                        before = self.keys(bucket)
                        between = r'''
progress_commits=0
mv() {
  case "$2" in
    */progress.json)
      progress_commits=$((progress_commits + 1))
      if [ "$progress_commits" = "$FAKE_PROGRESS_CRASH_AT" ] && [ "$FAKE_PROGRESS_CRASH_PHASE" = before ]; then kill -KILL $$; fi
      command mv "$@" || return 1
      if [ "$progress_commits" = "$FAKE_PROGRESS_CRASH_AT" ] && [ "$FAKE_PROGRESS_CRASH_PHASE" = after ]; then kill -KILL $$; fi ;;
    *) command mv "$@" ;;
  esac
}
'''
                        result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine', 'delete'],
                            between=between, env_extra={'FAKE_PROGRESS_CRASH_AT': str(boundary),
                                'FAKE_PROGRESS_CRASH_PHASE': phase})
                        self.assertNotEqual(result.returncode, 0)
                        original = re.findall(r'Plan (.*?): [0-9]+ exact keys', result.stdout)[-1]
                        tail = '. "$FAKE_HELPERS"; bucket=my-archive-bucket; prefix=agent-archive/; purge_resume "' + original + '" && purge_apply'
                        resumed = self.run_recipe(shell, 'agent-archive/', [], tail=tail)
                        if boundary == 2 and phase == 'after':
                            # Write ahead committed, but delete invocation is uncertain.
                            self.assertNotEqual(resumed.returncode, 0, resumed.stderr)
                            self.assertEqual(self.attempts(), [])
                            self.assertEqual(self.keys(bucket), before)
                        else:
                            self.assertEqual(resumed.returncode, 0, resumed.stderr)
                            self.assertEqual(self.keys(bucket), [k for k in before if not any(
                                k.startswith('agent-archive/sessions/' + d + '/') for d in ('claude/aaaa', 'cursor/dddd'))])

    def test_progress_capsule_commit_failure_stops_before_deletion(self):
        for shell in self.shells:
            for blocked in ('progress.next', 'progress.json'):
                with self.subTest(shell=shell, blocked=blocked):
                    bucket = self.build()
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine', 'delete'],
                        between='mkdir "$purge_dir/' + blocked + '"')
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.attempts(), [])
                    self.assertEqual(self.keys(bucket), before)

    def test_empty_listing_is_a_valid_noop(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', ''):
                with self.subTest(shell=shell, prefix=prefix):
                    bucket = self.build(prefix, sessions={})
                    result = self.run_recipe(shell, prefix, ['list', 'delete'])
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.keys(bucket), [])
                    self.assertEqual(self.attempts(), [])


if __name__ == '__main__':
    unittest.main()
