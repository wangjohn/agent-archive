#!/usr/bin/env python3
"""Require named tests to execute and pass; skip/absence is not acceptance."""
import argparse
import json
from pathlib import Path


def verify(path, required):
    outcomes = {}
    packages = {}
    failed = set()
    skipped = set()
    bad_packages = set()
    for line in path.read_text().splitlines():
        event = json.loads(line)
        action = event.get('Action')
        if action not in ('pass', 'fail', 'skip'):
            continue
        if event.get('Test'):
            name = event['Test']
            outcomes[name] = action
            if action == 'fail':
                failed.add(name)
            if action == 'skip' and any(name == parent or name.startswith(parent + '/') for parent in required):
                skipped.add(name)
        else:
            packages[event['Package']] = action
            if action != 'pass':
                bad_packages.add(event['Package'])
    missing = [name for name in required if outcomes.get(name) != 'pass']
    if missing or failed or skipped or bad_packages or not packages or any(value != 'pass' for value in packages.values()):
        raise ValueError(f'acceptance incomplete: required={missing}, failed={sorted(failed)}, skipped={sorted(skipped)}, bad_packages={sorted(bad_packages)}, packages={packages}')
    return {'executed': list(required), 'packages': packages, 'result': 'pass'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('path', type=Path)
    parser.add_argument('required', nargs='+')
    args = parser.parse_args()
    print(json.dumps(verify(args.path, args.required), sort_keys=True))


if __name__ == '__main__':
    main()
