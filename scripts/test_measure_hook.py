#!/usr/bin/env python3
"""Exercise hook benchmark validation without executing a CLI or using host homes."""
import contextlib
import io
import json
import pathlib
import runpy
import types
import unittest
from unittest import mock


class HookMeasurementTests(unittest.TestCase):
    def measure_stop(self, stop_effect):
        roots = set()

        def fake_run(command, *, input, env, **kwargs):
            home = pathlib.Path(env['AGENT_ARCHIVE_HOME'])
            roots.add(home.parent)
            self.assertEqual(pathlib.Path(env['HOME']).parent, home.parent)
            self.assertEqual(pathlib.Path(env['AWS_CONFIG_FILE']).parent, home.parent)
            self.assertEqual(pathlib.Path(env['AWS_SHARED_CREDENTIALS_FILE']).parent, home.parent)
            payload = json.loads(input)
            if payload['hook_event_name'] == 'SessionStart':
                registrations = home / 'registrations'
                registrations.mkdir(exist_ok=True)
                (registrations / 'synthetic.json').write_text('{}')
                requests = home / 'requests'
                requests.mkdir(exist_ok=True)
                (requests / 'synthetic.json').write_text(json.dumps({'reasons': ['sessionstart'], 'token': 'start', 'deferred': True}))
            elif payload['hook_event_name'] == 'Stop' and stop_effect is not None:
                (home / 'requests' / 'synthetic.json').write_text(json.dumps(stop_effect))
            return types.SimpleNamespace(returncode=0, stderr='', stdout='')

        try:
            with mock.patch('platform.platform', return_value='synthetic'), mock.patch('sys.argv', ['measure-hook.py', '/synthetic/binary', '--event', 'stop']), mock.patch('subprocess.run', side_effect=fake_run), contextlib.redirect_stdout(io.StringIO()):
                return runpy.run_path(str(pathlib.Path(__file__).with_name('measure-hook.py')))['report']
        finally:
            self.assertTrue(roots)
            self.assertTrue(all(not root.exists() for root in roots), 'temporary benchmark homes survived')

    def test_refuses_a_silent_stop_noop(self):
        with self.assertRaisesRegex(RuntimeError, 'Stop staged no urgent request'):
            self.measure_stop(None)

    def test_refuses_a_deferred_stop_request(self):
        with self.assertRaisesRegex(RuntimeError, 'Stop staged no urgent request'):
            self.measure_stop({'reasons': ['stop'], 'token': 'stop', 'deferred': True})

    def test_measures_an_urgent_stop_request(self):
        report = self.measure_stop({'reasons': ['stop'], 'token': 'stop'})
        self.assertEqual(set(report['scenarios']['stop']['results']), {'enabled', 'paused', 'disabled', 'ignored'})
        self.assertTrue(all(result['samples'] == 100 for result in report['scenarios']['stop']['results'].values()))


if __name__ == '__main__':
    unittest.main()
