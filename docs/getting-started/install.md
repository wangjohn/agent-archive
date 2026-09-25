# Install agent-archive

`agent-archive` is a single, self-contained macOS binary with no runtime
dependencies. It runs on Intel and Apple Silicon Macs.

> **Status: pre-release.** There is no signed release yet, so the install
> script and the release downloads below have nothing to fetch until the
> first `v0.1.0` tag is published. Until then, [build from
> source](#build-from-source).

After installing, continue with [setup](setup.md).

## Build from source

You need Go 1.27.1 (go.mod pins `toolchain go1.27.1`; any `go` command
from Go 1.21 on downloads it automatically) and Xcode's command line tools
(`xcode-select --install`), which the Keychain code needs through cgo.

```sh
git clone https://github.com/wangjohn/agent-archive.git
cd agent-archive
VERSION=dev ./scripts/build-release.sh
./dist/agent-archive-darwin-$(uname -m | sed 's/x86_64/amd64/') --version
```

`scripts/build-release.sh` is the script the release workflow uses, so a
local build has the release's build flags. Plain `go build
./cmd/agent-archive` also works for quick tests. Either way, a build from a
Git checkout reports `dev-<commit>` from `--version` (with `-dirty` when you
had uncommitted changes); put that in bug reports.

Put the binary on your `PATH` as plain `agent-archive`, for example:

```sh
mv ./dist/agent-archive-darwin-arm64 /opt/homebrew/bin/agent-archive   # Apple Silicon with Homebrew
# or: sudo mv ./dist/agent-archive-darwin-amd64 /usr/local/bin/agent-archive
```

A build runs on the Mac that built it. If you copy it to another Mac (AirDrop,
a browser download), macOS may quarantine the unsigned binary; right-click it
in Finder and choose **Open** once, or run `xattr -d com.apple.quarantine` on
it.

To upgrade, move or remove the old binary before putting the new one in
place. Overwriting it in place with `cp` can leave macOS killing it at launch
until the file is recreated. Setup records the binary's path in the hooks and
the LaunchAgent; if you move it, rerun `agent-archive setup` from the new
location (`status` reports hooks and background as `broken` until you do).

## Install with the script

Once a release exists:

```sh
curl -fsSL https://raw.githubusercontent.com/wangjohn/agent-archive/main/install.sh | sh
```

[`install.sh`](../../install.sh) is short; read it first if you prefer. It
downloads the release binary for your Mac's architecture, checks it against
the release's `SHA256SUMS` (which catches a damaged download; both files
come from the same release), checks that it carries a valid Developer ID
signature from the team the script names (which catches a binary someone
else built), and installs it as `agent-archive`, without `sudo`. Until the
first release the script names no team and refuses to install anything.
If `agent-archive` is already on your `PATH`, it replaces that copy,
so the hooks and background collector keep pointing at it. Otherwise it uses
`/usr/local/bin` when that is writable, and `~/.local/bin` if not, printing
the line to add to your shell profile when the directory isn't on your
`PATH`. It never runs setup.

Run the same command again to upgrade. To choose a release or a directory,
set the variables on the `sh` side of the pipe:

```sh
curl -fsSL https://raw.githubusercontent.com/wangjohn/agent-archive/main/install.sh | AGENT_ARCHIVE_VERSION=v0.1.0 AGENT_ARCHIVE_INSTALL_DIR="$HOME/bin" sh
```

## Install a release build by hand

1. Download the binary for your Mac from the
   [latest release](https://github.com/wangjohn/agent-archive/releases/latest)
   (`agent-archive-darwin-arm64` for Apple Silicon, `agent-archive-darwin-amd64`
   for Intel), and `SHA256SUMS` from the same release.
2. Verify the checksum:

   ```sh
   shasum -a 256 -c SHA256SUMS --ignore-missing
   ```

3. Make it executable and put it on your `PATH` as `agent-archive`:

   ```sh
   chmod +x agent-archive-darwin-*
   sudo mv agent-archive-darwin-* /usr/local/bin/agent-archive
   ```

   On an Apple Silicon Mac with Homebrew, `/usr/local/bin` may not exist;
   create it (`sudo mkdir -p /usr/local/bin`) or use `/opt/homebrew/bin`,
   which needs no `sudo`.

4. Confirm it runs: `agent-archive --version`.

Release binaries are signed with a Developer ID and notarized by Apple.
Gatekeeper may need network access to check the notarization ticket on first
launch. To check a download yourself, `codesign --verify --strict
--verbose=2 agent-archive-darwin-arm64` checks the signature, `codesign -dv
agent-archive-darwin-arm64` shows the `TeamIdentifier` (compare it with
`team_id` in `install.sh`), and, with the GitHub CLI, `gh attestation verify
agent-archive-darwin-arm64 --repo wangjohn/agent-archive` checks that the
file was built by this repository's release workflow.

## Next

- [Set up capture](setup.md): choose apps and projects, connect a bucket,
  start the background collector.
- [Import sessions already on this Mac](../guides/backfill.md).
- [Uninstall](uninstall.md).
