#!/usr/bin/env python3
"""Issue only object and listing authority under one disposable run prefix."""
import json
import re
import sys


def policy(prefix):
    if not re.fullmatch(r'aa-provider-[0-9a-f]{16}', prefix):
        raise ValueError('invalid disposable run prefix')
    bucket = 'arn:aws:s3:::aa-disposable-acceptance'
    return {'Version': '2012-10-17', 'Statement': [
        {'Effect': 'Allow', 'Action': ['s3:GetBucketLocation'], 'Resource': [bucket]},
        {'Effect': 'Allow', 'Action': ['s3:ListBucket'], 'Resource': [bucket],
         'Condition': {'StringLike': {'s3:prefix': [prefix + '/*']}}},
        {'Effect': 'Allow', 'Action': ['s3:GetObject', 's3:PutObject', 's3:DeleteObject'],
         'Resource': [bucket + '/' + prefix + '/*']},
    ]}


if __name__ == '__main__':
    print(json.dumps(policy(sys.argv[1]), sort_keys=True))
