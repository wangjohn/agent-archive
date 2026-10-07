"""Harness correctness checks; never substitute these for real MinIO execution."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
PROVIDER = ROOT / 'scripts' / 'acceptance' / 'provider'


def module(name):
    spec = importlib.util.spec_from_file_location(name, PROVIDER / f'{name}.py')
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


class EvidenceVerifierTest(unittest.TestCase):
    def test_missing_skipped_failed_and_package_failure_never_pass(self):
        verifier = module('verify-results')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            for action in ('skip', 'fail', None):
                events = [{'Action': 'pass', 'Package': 'p'}]
                if action:
                    events.append({'Action': action, 'Package': 'p', 'Test': 'required'})
                path.write_text('\n'.join(json.dumps(event) for event in events))
                with self.assertRaises(ValueError):
                    verifier.verify(path, ['required'])
            path.write_text('\n'.join(json.dumps(event) for event in (
                {'Action': 'pass', 'Package': 'p', 'Test': 'required'},
                {'Action': 'fail', 'Package': 'p'},
            )))
            with self.assertRaises(ValueError):
                verifier.verify(path, ['required'])

    def test_later_pass_never_erases_prior_failed_or_skipped_evidence(self):
        verifier = module('verify-results')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            for bad in ('fail', 'skip'):
                for name in ('required', None):
                    events = [{'Action': bad, 'Package': 'p'}]
                    if name:
                        events[0]['Test'] = name
                    events.extend((
                        {'Action': 'pass', 'Package': 'p', 'Test': 'required'},
                        {'Action': 'pass', 'Package': 'p'},
                    ))
                    path.write_text('\n'.join(json.dumps(event) for event in events))
                    with self.assertRaises(ValueError):
                        verifier.verify(path, ['required'])

    def test_success_requires_actual_named_and_package_pass(self):
        verifier = module('verify-results')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            path.write_text('\n'.join(json.dumps(event) for event in (
                {'Action': 'pass', 'Package': 'p', 'Test': 'required'},
                {'Action': 'pass', 'Package': 'p'},
            )))
            self.assertEqual(verifier.verify(path, ['required'])['result'], 'pass')

    def test_passed_parent_with_skipped_required_subtest_is_rejected(self):
        verifier = module('verify-results')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            path.write_text('\n'.join(json.dumps(event) for event in (
                {'Action': 'skip', 'Package': 'p', 'Test': 'required/unavailable'},
                {'Action': 'pass', 'Package': 'p', 'Test': 'required'},
                {'Action': 'pass', 'Package': 'p'},
            )))
            with self.assertRaises(ValueError):
                verifier.verify(path, ['required'])

    def test_passed_parent_without_required_descendant_is_rejected(self):
        verifier = module('verify-results')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            path.write_text('\n'.join(json.dumps(event) for event in (
                {'Action': 'pass', 'Package': 'p', 'Test': 'required'},
                {'Action': 'pass', 'Package': 'p'},
            )))
            with self.assertRaises(ValueError):
                verifier.verify(path, ['required', 'required/expected'])

    def test_credential_redaction_fails_acceptance_and_preserves_other_evidence(self):
        sanitizer = module('sanitize-evidence')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'artifact.json'
            path.write_bytes(b'{"error":"ephemeral-private","result":"fail"}')
            self.assertTrue(sanitizer.sanitize(Path(directory), [b'ephemeral-private']))
            self.assertNotIn(b'ephemeral-private', path.read_bytes())
            self.assertIn(b'"result":"fail"', path.read_bytes())


    def test_admin_credentials_are_sanitized_and_fail_the_gate(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'provider-service.txt'
            path.write_text('private-admin-secret')
            result = subprocess.run(['python3', str(PROVIDER / 'sanitize-evidence.py'), directory],
                                    env=dict(os.environ, AA_PROVIDER_ADMIN_SECRET='private-admin-secret'),
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertNotIn('private-admin-secret', path.read_text())
            self.assertIn('acceptance failed closed', result.stderr)


class ProviderHarnessPolicyTest(unittest.TestCase):
    def test_refuses_local_host_before_any_service_mutation(self):
        env = {'PATH': os.environ['PATH']}
        result = subprocess.run(['bash', str(PROVIDER / 'host.sh')], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn('disposable non-root Linux CI runner', result.stderr)

    def test_both_sdk_identities_have_only_run_prefix_authority(self):
        script = (PROVIDER / 'host.sh').read_text()
        self.assertIn('"$admin_access" "$admin_secret"', script)
        policy = module('credential-policy').policy('aa-provider-0123456789abcdef')
        statements = policy['Statement']
        self.assertEqual(statements[0]['Action'], ['s3:GetBucketLocation'])
        self.assertEqual(statements[1]['Condition'], {'StringLike': {'s3:prefix': ['aa-provider-0123456789abcdef/*']}})
        self.assertEqual(statements[2]['Resource'], ['arn:aws:s3:::aa-disposable-acceptance/aa-provider-0123456789abcdef/*'])
        self.assertEqual(statements[2]['Action'], ['s3:GetObject', 's3:PutObject', 's3:DeleteObject'])
        self.assertIn('MINIO_ROOT_USER=%s', script)
        self.assertIn('--user "$access"', script)
        self.assertIn('--user "$peer_access"', script)
        self.assertIn('AA_PROVIDER_RUN_PREFIX="$prefix"', script)
        for invalid in ('', '../outside', 'aa-provider-*', 'aa-provider-0123456789abcdef/other'):
            with self.assertRaises(ValueError):
                module('credential-policy').policy(invalid)

    def test_script_parses_and_provider_source_versions_are_exact(self):
        subprocess.run(['bash', '-n', str(PROVIDER / 'host.sh')], check=True)
        script = (PROVIDER / 'host.sh').read_text()
        self.assertIn('TestProviderPurgeAmbiguousSelectingMetadataFailsClosed/incomplete', script)
        self.assertIn('TestProviderRestorationMissingOrChangedIntentRetainsPending/changed', script)
        self.assertIn('github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c', script)
        self.assertIn('github.com/minio/mc@v0.0.0-20251106162529-77f82e18b540', script)
        self.assertIn('--publish 127.0.0.1::9000', script)
        self.assertIn('CGO_ENABLED=0 go install', script)
        self.assertNotIn('export GOBIN="$work/tools" GOTOOLCHAIN=local CGO_ENABLED=0', script)
        self.assertIn("trap cleanup EXIT", script)
        self.assertNotIn('docker system prune', script)
        self.assertNotIn('docker ps', script)

    def test_ci_targets_exact_head_and_keeps_required_existing_workflow(self):
        workflow = (ROOT / '.github' / 'workflows' / 'provider-runtime.yml').read_text()
        self.assertIn('  pull_request:', workflow)
        self.assertEqual(workflow.count('ref: ${{ github.event.pull_request.head.sha || github.sha }}'), 2)
        self.assertEqual(workflow.count('persist-credentials: false'), 2)
        self.assertEqual(workflow.count('if: always()'), 3)
        self.assertIn('macos-15-intel', workflow)
        self.assertIn('macos-14', workflow)
        self.assertIn('ubuntu-24.04', workflow)
        self.assertNotIn('continue-on-error', workflow)
        self.assertNotIn('secrets.', workflow)
        self.assertIn('TestDurableLiveCursorAdmissionPublishesAfterSourceDeletion', workflow)
        self.assertIn('scripts/test_published_writer.sh', workflow)
        self.assertIn("run: bash scripts/acceptance/provider/cleanup.sh", workflow)
        self.assertNotIn('release-candidate/', workflow)


class OwnedCleanupTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        tools = self.root / 'bin'
        tools.mkdir()
        self.prefix = 'aa-provider-0123456789abcdef'
        self.work = self.root / self.prefix
        self.work.mkdir()
        self.control = self.root / 'aa-provider-resources'
        self.control.write_text(self.prefix + '\n')
        self.log = self.root / 'calls'
        for name, body in {
            'uname': '#!/bin/sh\necho Linux\n',
            'id': '#!/bin/sh\necho 1000\n',
            'docker': r"""#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_LOG"
case "$1 $2" in
  'info ') [ -z "$FAKE_DAEMON_ERROR" ]; exit $? ;;
  'container inspect'|'image inspect')
    [ -z "$FAKE_INSPECT_ERROR" ] || { echo 'transport failure' >&2; exit 1; }
    [ ! -e "$RUNNER_TEMP/$3.exists" ] || exit 0
    if [ "$1" = image ] && [ -n "$FAKE_IMAGE_LATEST" ]; then
      echo "Error response from daemon: No such image: $3:latest" >&2
    else echo "Error: No such object: $3" >&2; fi
    exit 1 ;;
  'rm -f')
    [ -z "$FAKE_REMOVE_ERROR" ] || exit 4
    rm "$RUNNER_TEMP/$3.exists"; exit $? ;;
  'image rm')
    [ -z "$FAKE_REMOVE_ERROR" ] || exit 4
    rm "$RUNNER_TEMP/$3.exists"; exit $? ;;
esac
exit 9
""",
        }.items():
            path = tools / name
            path.write_text(body)
            path.chmod(0o700)
        self.env = dict(os.environ, GITHUB_ACTIONS='true', RUNNER_TEMP=str(self.root),
                        PATH=str(tools) + ':/usr/bin:/bin', FAKE_LOG=str(self.log))
        for suffix in ('service', 'image'):
            (self.root / f'{self.prefix}-{suffix}.exists').touch()

    def run_cleanup(self, **extra):
        return subprocess.run(['bash', str(PROVIDER / 'cleanup.sh')],
                              env=dict(self.env, **extra), capture_output=True, text=True)

    def test_success_removes_only_owned_named_resources(self):
        result = self.run_cleanup()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.control.exists())
        self.assertFalse(self.work.exists())
        calls = self.log.read_text().splitlines()
        self.assertIn(f'rm -f {self.prefix}-service', calls)
        self.assertIn(f'image rm {self.prefix}-image', calls)
        self.assertFalse(any('prune' in call or call.startswith('ps') for call in calls))

    def test_docker_normalized_latest_absence_is_exactly_owned(self):
        result = self.run_cleanup(FAKE_IMAGE_LATEST="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.control.exists())

    def test_uncertain_daemon_or_removal_keeps_recovery_control(self):
        for failure in ('FAKE_DAEMON_ERROR', 'FAKE_REMOVE_ERROR', 'FAKE_INSPECT_ERROR'):
            result = self.run_cleanup(**{failure: '1'})
            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(self.control.exists())
            self.assertTrue(self.work.exists())
        result = self.run_cleanup()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.control.exists())

    def test_unowned_control_never_runs_docker(self):
        self.control.write_text('someone-elses-container\n')
        result = self.run_cleanup()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.log.exists())
        self.assertTrue(self.work.exists())


if __name__ == '__main__':
    unittest.main()
