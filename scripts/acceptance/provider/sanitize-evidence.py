#!/usr/bin/env python3
"""Remove ephemeral credential values before any acceptance artifact upload."""
import os
from pathlib import Path
import sys


def sanitize(root, secrets):
    found = False
    for path in root.iterdir():
        if not path.is_file() or path.is_symlink():
            raise ValueError('unexpected acceptance artifact entry')
        data = path.read_bytes()
        for secret in secrets:
            if secret and secret in data:
                data = data.replace(secret, b'[ephemeral credential removed]')
                found = True
        path.write_bytes(data)
    return found


if __name__ == '__main__':
    values = [os.environ.get(name, '').encode() for name in ('AA_PROVIDER_ACCESS', 'AA_PROVIDER_SECRET', 'AA_PROVIDER_PEER_ACCESS', 'AA_PROVIDER_PEER_SECRET')]
    if sanitize(Path(sys.argv[1]), values):
        print('Credential-bearing evidence was sanitized; acceptance failed closed.', file=sys.stderr)
        sys.exit(1)
