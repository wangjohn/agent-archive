#!/usr/bin/env python3
"""Require named tests to execute and pass; skip/absence is not acceptance."""
import argparse
import json
from pathlib import Path


def verify(path, required):
    outcomes = {}
    packages = {}
    for line in path.read_text().splitlines():
        event = json.loads(line)
        action = event.get('Action')
        if action not in ('pass', 'fail', 'skip'):
            continue
        if event.get('Test'):
            outcomes[event['Test']] = action
        else:
            packages[event['Package']] = action
    missing = [name for name in required if outcomes.get(name) != 'pass']
    failed = [name for name, action in outcomes.items() if action == 'fail']
    if missing or failed or not packages or any(value != 'pass' for value in packages.values()):
        raise ValueError(f'acceptance incomplete: required={missing}, failed={failed}, packages={packages}')
    return {'executed': list(required), 'packages': packages, 'result': 'pass'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('path', type=Path)
    parser.add_argument('required', nargs='+')
    args = parser.parse_args()
    print(json.dumps(verify(args.path, args.required), sort_keys=True))


if __name__ == '__main__':
    main()
