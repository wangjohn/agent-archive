#!/usr/bin/env python3
"""Native published-release refusal checks against an isolated loopback S3 fixture.

No R2/Keychain path, credential helpers, native sessions, scheduler service or
real provider is used. These fake endpoint checks do not establish S3 acceptance.
"""
import argparse
import hashlib
import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import urllib.parse
from xml.sax.saxutils import escape

PINS = {
    "v0.1.0": "43fbe6d8d65d2d32ebad66d55d116e10d517c40908032297bd5977beb45ab5ad",
    "v0.1.1": "c8fff68b623a7e0143503adce2d83c6494d7efa55fa0724eb4f76ae54c66255e",
}
SESSION = "a" * 32
PREFIX = "sessions/codex/" + SESSION + "/"
STAMP = "2020-01-01T00:00:00Z"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifacts", required=True, type=Path)
    args = parser.parse_args()
    assert os.uname().sysname == "Darwin" and os.uname().machine == "x86_64"
    for version, expected in PINS.items():
        binary = args.artifacts.resolve() / version / "agent-archive-darwin-amd64"
        assert hashlib.sha256(binary.read_bytes()).hexdigest() == expected
        with tempfile.TemporaryDirectory(prefix="published-history-", dir="/private/tmp") as tmp:
            run_release(binary, version, Path(tmp))


def run_release(binary, version, root):
    home, state, temp, bins = [root / p for p in ("home", "state", "tmp", "bin")]
    for p in (home, state, temp, bins):
        p.mkdir(mode=0o700)
    for name in ("launchctl", "systemctl", "security", "git", "getconf"):
        p = bins / name
        p.write_text('#!/bin/sh\necho "$0 $*" >> "$HOME/unexpected-service"\nexit 99\n')
        p.chmod(0o700)
    aws = home / ".aws"
    aws.mkdir(mode=0o700)
    (aws / "credentials").write_text("[synthetic]\naws_access_key_id = SYNTHETIC\naws_secret_access_key = SYNTHETIC\n")
    (aws / "config").write_text("[profile synthetic]\nregion = us-east-1\n")
    env = {"HOME": str(home), "AGENT_ARCHIVE_HOME": str(state), "TMPDIR": str(temp),
           "PATH": str(bins), "CODEX_HOME": str(home / ".codex"),
           "CLAUDE_CONFIG_DIR": str(home / ".claude"), "NO_COLOR": "1",
           "AWS_CONFIG_FILE": str(aws / "config"), "AWS_SHARED_CREDENTIALS_FILE": str(aws / "credentials"),
           "AWS_EC2_METADATA_DISABLED": "true", "AWS_MAX_ATTEMPTS": "1"}
    objects = {PREFIX + "source." + c * 64 + ".jsonl.gz": ("synthetic-" + c).encode() for c in "abc"}
    sources = list(objects)
    meta = {"schema_version": 2, "session_id": SESSION, "native_session_id": "native-synthetic",
            "project_id": "project-synthetic", "machine_id": "synthetic", "harness": {"name": "codex"},
            "captured_at": STAMP, "filter_version": "12",
            "source_bundle": {"key": sources[0], "sha256": "a" * 64, "compressed_bytes": len(objects[sources[0]])},
            "history": {"current_revision": "11111111-1111-4111-8111-111111111111", "preserved": [
                {"revision_id": str(i) * 8 + "-" + str(i) * 4 + "-4" + str(i) * 3 + "-8" + str(i) * 3 + "-" + str(i) * 12,
                 "captured_at": STAMP, "source": {"key": sources[i-1], "sha256": "abc"[i-1] * 64,
                                                     "compressed_bytes": len(objects[sources[i-1]])}}
                for i in (2, 3)]}}
    objects[PREFIX + "metadata.json"] = json.dumps(meta).encode()
    frozen = dict(objects)
    operations = []

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def serve(self, head=False):
            url = urllib.parse.urlsplit(self.path)
            query = urllib.parse.parse_qs(url.query, keep_blank_values=True)
            key = urllib.parse.unquote(url.path).removeprefix("/synthetic/")
            operations.append((self.command, key))
            if "list-type" in query:
                prefix = query.get("prefix", [""])[0]
                contents = "".join('<Contents><Key>' + escape(k) + '</Key><LastModified>' + STAMP +
                                   '</LastModified><ETag>"' + hashlib.md5(v).hexdigest() + '"</ETag><Size>' +
                                   str(len(v)) + '</Size><StorageClass>STANDARD</StorageClass></Contents>'
                                   for k, v in objects.items() if k.startswith(prefix))
                payload = ('<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>synthetic</Name>'
                           '<IsTruncated>false</IsTruncated>' + contents + '</ListBucketResult>').encode()
                self.send_response(200)
            elif self.command == "PUT":
                objects[key] = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                payload = b""
                self.send_response(200)
            elif self.command == "DELETE":
                objects.pop(key, None)
                payload = b""
                self.send_response(204)
            elif key in objects:
                payload = objects[key]
                self.send_response(200)
                self.send_header("ETag", '"' + hashlib.md5(payload).hexdigest() + '"')
                self.send_header("Last-Modified", "Thu, 01 Jan 2026 00:00:00 GMT")
            else:
                payload = b"<Error><Code>NoSuchKey</Code></Error>"
                self.send_response(404)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            if not head:
                self.wfile.write(payload)

        def do_GET(self):
            self.serve()

        def do_HEAD(self):
            self.serve(True)

        def do_PUT(self):
            self.serve()

        def do_DELETE(self):
            self.serve()

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    endpoint = "http://127.0.0.1:" + str(server.server_port)
    env["AWS_ENDPOINT_URL"] = endpoint
    env["AWS_ENDPOINT_URL_S3"] = endpoint
    cfg = {"schema_version": 1, "machine_id": "synthetic", "retention_days": 1,
           "storage": {"Provider": "s3", "AWSProfile": "synthetic", "Region": "us-east-1", "Bucket": "synthetic"},
           "archive": {"enabled": True}, "harnesses": ["codex"]}
    (state / "config.json").write_text(json.dumps(cfg))
    try:
        def command(*argv):
            before = len(operations)
            proc = subprocess.run([str(binary), *argv], cwd=home, env=env, stdin=subprocess.DEVNULL,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=25)
            output = proc.stdout.decode(errors="replace")
            assert not (home / "unexpected-service").exists(), output
            assert all(objects.get(k) == v for k, v in frozen.items()), "protected archive changed"
            assert not any(method in ("PUT", "DELETE") and key.startswith(PREFIX) for method, key in operations), "protected mutation attempted"
            print(json.dumps({"release": version, "command": list(argv), "exit": proc.returncode,
                              "operations": operations[before:], "output": output.strip()}))
            return proc, output

        proc, output = command("purge")
        assert proc.returncode != 0 and "unknown command" in output.lower()
        proc, output = command("show", SESSION, "--harness", "codex")
        assert proc.returncode != 0 and "unsupported metadata schema version 2" in output
        proc, output = command("list", "--no-cache")
        assert "unsupported metadata schema version 2" in output
        for p in (state / "registrations", state / "published"):
            p.mkdir(exist_ok=True)
        reg = {"archive_session_id": SESSION, "native_session_id": "native-synthetic", "project_id": "project-synthetic",
               "project_root": str(home / "project"), "harness": {"name": "codex"},
               "transcript_path": str(home / "missing-native.jsonl"), "session_started_at": STAMP,
               "registered_at": STAMP, "admitted_at": STAMP}
        (state / "registrations" / (SESSION + ".json")).write_text(json.dumps(reg))
        bundle = {"schema_version": 2, "archive_session_id": SESSION, "native_session_id": "native-synthetic",
                  "project_id": "project-synthetic", "capture": {"harness": {"name": "codex"},
                  "adapter_name": "codex", "adapter_version": "1", "filter_version": "12", "captured_at": STAMP}, "native_records": []}
        published = {"bundle": bundle, "status": "published", "published_at": STAMP,
                     "summary": {"harness": "codex", "status": "published", "captured_at": STAMP,
                                 "published": True, "last_published_at": STAMP}}
        (state / "published" / (SESSION + ".json")).write_text(json.dumps(published))
        before = len(operations)
        _, output = command("sync")
        assert any(method == "GET" and key == PREFIX + "metadata.json" for method, key in operations[before:]), "retention did not read selecting metadata"
        assert "unsupported metadata schema version 2" in output, "retention schema refusal not observed"
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    main()
