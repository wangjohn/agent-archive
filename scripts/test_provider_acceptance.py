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

    def test_success_requires_actual_named_and_package_pass(self):
        verifier = module('verify-results')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            path.write_text('\n'.join(json.dumps(event) for event in (
                {'Action': 'pass', 'Package': 'p', 'Test': 'required'},
                {'Action': 'pass', 'Package': 'p'},
            )))
            self.assertEqual(verifier.verify(path, ['required'])['result'], 'pass')

    def test_credential_redaction_fails_acceptance_and_preserves_other_evidence(self):
        sanitizer = module('sanitize-evidence')
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'artifact.json'
            path.write_bytes(b'{"error":"ephemeral-private","result":"fail"}')
            self.assertTrue(sanitizer.sanitize(Path(directory), [b'ephemeral-private']))
            self.assertNotIn(b'ephemeral-private', path.read_bytes())
            self.assertIn(b'"result":"fail"', path.read_bytes())


class ProviderHarnessPolicyTest(unittest.TestCase):
    def test_refuses_local_host_before_any_service_mutation(self):
        env = {'PATH': os.environ['PATH']}
        result = subprocess.run(['bash', str(PROVIDER / 'host.sh')], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn('disposable non-root Linux CI runner', result.stderr)

    def test_script_parses_and_provider_source_versions_are_exact(self):
        subprocess.run(['bash', '-n', str(PROVIDER / 'host.sh')], check=True)
        script = (PROVIDER / 'host.sh').read_text()
        self.assertIn('github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c', script)
        self.assertIn('github.com/minio/mc@v0.0.0-20251106162529-77f82e18b540', script)
        self.assertIn('--publish 127.0.0.1::9000', script)
        self.assertIn("trap cleanup EXIT", script)
        self.assertNotIn('docker system prune', script)
        self.assertNotIn('docker ps', script)

    def test_ci_targets_exact_head_and_keeps_required_existing_workflow(self):
        workflow = (ROOT / '.github' / 'workflows' / 'provider-runtime.yml').read_text()
        self.assertIn('  pull_request:', workflow)
        self.assertEqual(workflow.count('ref: ${{ github.event.pull_request.head.sha || github.sha }}'), 2)
        self.assertEqual(workflow.count('persist-credentials: false'), 2)
        self.assertEqual(workflow.count('if: always()'), 2)
        self.assertIn('macos-15-intel', workflow)
        self.assertIn('macos-14', workflow)
        self.assertIn('ubuntu-24.04', workflow)
        self.assertNotIn('continue-on-error', workflow)
        self.assertNotIn('secrets.', workflow)
        self.assertIn('TestDurableLiveCursorAdmissionPublishesAfterSourceDeletion', workflow)
        self.assertIn('scripts/test_published_writer.sh', workflow)
        self.assertNotIn('release-candidate/', workflow)


if __name__ == '__main__':
    unittest.main()
