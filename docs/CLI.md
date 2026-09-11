# aispace CLI reference

For a task-oriented walkthrough of sealed bundles, agent identities, addressed inboxes, device
pairing, adaptive durable-first fallback, and recovery, read
[Secure handoffs](SECURE_HANDOFFS.md).

`aispace` is a single static Go binary. It wraps the bot API in [API.md](API.md) and is designed
to be driven by humans in a terminal and by LLM agents through a shell tool. Most commands support
`--json`; some return a documented client-composed result, while secret-bearing interactive
handoff commands deliberately reject JSON mode.

## Install

```sh
curl -fsSL https://aispace.sh/install.sh | sh
```

The installer detects OS/arch (darwin/linux, amd64/arm64), downloads the latest `v*` release
from `github.com/aispace-sh/aispace-client`, verifies the binary against `checksums.txt` (SHA-256) and
places it in `/usr/local/bin` (or `~/.local/bin` if that is not writable). Override with
`AISPACE_INSTALL_DIR=/path`. Manual install: download the asset for your platform from
[GitHub Releases](https://github.com/aispace-sh/aispace-client/releases), `chmod +x`, move to `$PATH`.

On Windows x64 or ARM64, install through npm; it downloads the matching checksummed `.exe` release:

```powershell
npm install -g @aispace-sh/cli
aispace version
```

Windows binaries are checksum-verified but not currently code-signed, so Windows may show a
SmartScreen warning on first run.

Build from source:

```sh
go build -o aispace . && ./aispace version
```

## Configuration

Resolution order (first match wins):

| Source | Key | URL |
|---|---|---|
| Flag | `--key` | `--url` |
| Environment | `AISPACE_KEY` | `AISPACE_URL` |
| Config file | `~/.config/aispace/config.json` → `key` | → `url` |
| Default | — | `https://aispace.sh` |

Config file (mode `0600`, directory `0700`), written by `aispace login`:

```json
{ "url": "https://aispace.sh", "key": "ask_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX" }
```

`$XDG_CONFIG_HOME/aispace/config.json` is used when `XDG_CONFIG_HOME` is set. Ephemeral
environments (CI, agent sandboxes) can skip `login` entirely and export `AISPACE_KEY`.

| Env var | Purpose |
|---|---|
| `AISPACE_KEY` | Bot key; overrides the config file |
| `AISPACE_URL` | Base URL; overrides the config file (self-hosted or `http://localhost:8787`) |
| `AISPACE_AGE_IDENTITY` | Secret age X25519 identity used by `decrypt` when `--identity-file` is omitted |
| `AISPACE_TRANSFER_TOKEN` | Sealed recipient link or canonical token used by `transfer receive` |
| `AISPACE_CONFIG` | Full path to the config file, overriding the `XDG_CONFIG_HOME`/`HOME` lookup |
| `AISPACE_INSTALL_DIR` | Installer only: destination directory |

## Global flags

| Flag | Meaning |
|---|---|
| `--json` | Print documented machine-readable JSON where the command supports it |
| `--key <ask_...>` | Override the key for this invocation |
| `--url <base>` | Override base URL for this invocation |
| `-h, --help` | Help |

`--json`, `--key` and `--url` are persistent flags, so they work on every subcommand — `--key` is
not limited to `login`.

Human-readable output goes to **stdout**; warnings go to **stderr**; errors go to **stderr** as a
single line `error: <message> (<code>)`:

```
error: Invalid key (invalid_key)
```

In `--json` mode stdout stays empty and the error is written to stderr as one object that wraps
the API's code and message together with the exit code, so a caller can branch on either:

```json
{"error":{"code":"invalid_key","exit_code":3,"message":"Invalid key","status":401}}
```

`details` and `retry_after` are included when the server supplies them.

## Exit codes

| Code | Meaning | Typical cause |
|---|---|---|
| `0` | Success | |
| `1` | Generic failure | Network error, 5xx, unexpected response, file not found locally |
| `2` | Usage error | Bad flag, missing argument, unparsable duration, control character in the key, `--name` or `--content-type` |
| `3` | Authentication | No key configured, `401 invalid_key`, `401 key_revoked` |
| `4` | Quota | `402 quota_exceeded`, `402 allowance_exceeded`, `402 payment_required`, `400` or `413 file_too_large` |
| `5` | Rate limited or monthly cap | `429 rate_limited` after the retry policy below gave up; `429 monthly_upload_cap` / `429 monthly_download_cap` (never retried — the reset is next month) |

Retry policy: the CLI retries **once** on either of two conditions:

- `429 rate_limited` for idempotent `GET`s (`ls`, `quota`, `download` and metadata lookups), waiting
  the `Retry-After` the server gives (capped at 30s).
- `502`, `503` or `504` for non-metered metadata `GET`s. The CLI does not replay authenticated
  downloads after a gateway error because each request consumes monthly download allowance and the
  first attempt may already have been counted.

`500` is **not** retried: it usually means the request itself is the problem, so a second attempt
doubles the load and returns the same error. Writes are never retried either — a `503` may still
have been applied, and repeating an upload could store the file twice. `monthly_upload_cap` and
`monthly_download_cap` are never retried — `Retry-After` points at the start of the next UTC month
— and exit `5` is returned immediately with the server's message (which points at
contacts@aispace.sh). Uploads, link creation and deletes are never
retried automatically; the agent decides. `Retry-After` is echoed in the error message.

## Timeouts

There is **no deadline on a transfer as a whole**. A large file over a slow link is slow, not
broken. Instead, uploads and downloads have a two-minute inactivity timeout that resets whenever
body bytes move. A transfer can run for hours while making progress, but a connected peer that
stops moving data does not hold an unattended process forever.

What is bounded are the phases before any bytes flow, so an unreachable or silent server is still
given up on quickly:

| Phase | Limit |
|---|---|
| TCP connect | 30s |
| TLS handshake | 10s |
| Waiting for response headers | 60s |
| Upload/download with no byte progress | 2m |

A transfer that trips the inactivity timer fails with exit `1` and code `timeout`, whether it
stalled before the body started or part-way through it:

```
error: transfer stalled without byte progress (timeout)
error: transfer stalled (timeout)
```

A partly written output file is removed, so a stalled download never leaves a truncated file
behind.

`Ctrl-C`/`SIGTERM` cancels in-flight requests, closes stdin to unblock ordinary pipes, and exits `1`
with code `interrupted`. After the first signal, default handling is restored so a second interrupt
can terminate a source that cannot be closed cleanly.

## Durations

Flags that take a duration accept Go-style strings with an added `d` unit: `30s`, `15m`, `1h`,
`36h`, `7d`. Plain integers are seconds. File lifetimes above the plan maximum are rejected: 7d on
Free and 30d on Pro. Public links require Pro, are clamped to its 30d maximum, and never outlive
their file. The CLI prints the effective expiry returned by the server.

## Commands

### MCP server

`aispace mcp serve` runs the native client as a local stdio Model Context Protocol server. It
exposes exactly ten tools for identity/quota inspection, bounded file and link listing, private-by-
default upload, verified download, explicit public-link creation, revocation, and deletion. It
prints protocol messages only to stdout and stops on stdin EOF or cancellation.

Authentication is resolved from `AISPACE_KEY` before the existing mode-0600 client configuration.
The service origin comes from `AISPACE_URL`, configured URL, or `https://aispace.sh`. Plain HTTP is
accepted only for an explicitly configured loopback development server. `--key` is rejected for
this command because process arguments may be visible to other users.

Local upload and download paths are limited by `AISPACE_ALLOWED_ROOTS`, then intersected with MCP
roots supplied by the host. Without either, access is limited to the canonical working directory.
Downloads verify their recorded SHA-256 digest, do not overwrite by default, and publish the final
file only after verification.

Codex configuration:

```toml
[mcp_servers.aispace]
command = "aispace"
args = ["mcp", "serve"]
env_vars = ["AISPACE_KEY", "AISPACE_URL", "AISPACE_ALLOWED_ROOTS"]
startup_timeout_sec = 10
tool_timeout_sec = 120
default_tools_approval_mode = "writes"

[mcp_servers.aispace.tools.aispace_create_link]
approval_mode = "prompt"
[mcp_servers.aispace.tools.aispace_revoke_link]
approval_mode = "prompt"
[mcp_servers.aispace.tools.aispace_delete_file]
approval_mode = "prompt"
```

Claude Code user configuration (the single quotes prevent the shell from expanding the key):

```sh
claude mcp add-json --scope user aispace \
  '{"type":"stdio","command":"aispace","args":["mcp","serve"],"env":{"AISPACE_KEY":"${AISPACE_KEY}"}}'
claude mcp get aispace
```

Generic stdio host configuration:

```json
{
  "mcpServers": {
    "aispace": {
      "command": "aispace",
      "args": ["mcp", "serve"],
      "env": { "AISPACE_KEY": "<injected by the host secret store>" }
    }
  }
}
```

Never commit an expanded bot key. Use a distinct scoped key and budget for each agent or host.

### `aispace transfer create`

```sh
aispace transfer create <path> [path...] --sealed --link \
  [--expires 1d] [--max-downloads 1]
```

Transport selection is explicit and preserves the same sealed ciphertext format:

```sh
aispace transfer create report.pdf --sealed --link \
  --transport adaptive --durability durable-first --transport-privacy relay-only
```

`stored` remains the default. In the current experimental release, the Go client records adaptive
intent but has no reviewed WebRTC/TURN byte driver, so it immediately continues over durable R2 and
prints the fallback reason. It sends adaptive-only create fields only after successful compatible
capability discovery; older, malformed, or temporarily unavailable discovery endpoints fall back
to the strict stored request. `direct-first` and `live-only` are rejected rather than silently
changing durability. Choose `--transport-privacy direct` only when accepting that a future direct
path may expose network addresses to the peer and signaling infrastructure; `relay-only` is the
safer default.

Creates one asynchronous, end-to-end encrypted transfer. Every input must be a regular file;
filenames, content types, per-file sizes, and plaintext hashes exist only in the encrypted
manifest. The service sees aggregate quota and multipart values. Content uses independently
authenticated 8 MiB AES-256-GCM chunks, so an interrupted upload or download can restart at a
known ciphertext boundary without trusting partial plaintext. Upload resume survives a later CLI
invocation; download range retry currently covers transient failures within the active invocation.

V1 accepts at most 1,000 files, 10,000 ciphertext parts, and a 1 MiB encrypted manifest.

The command prints both an HTTPS recipient link and a canonical CLI token. The link has the form
`https://aispace.sh/t/<id>#as1.<master-key>.<claim-capability>`. Everything after `#` is a bearer
secret: the browser does not send it in HTTP, and the CLI sends only the claim capability in an
Authorization header. The master key never leaves the sender or recipient.

Human output continues to print the recipient link. JSON output omits both bearer `link` and
`token` by default; automation must opt in with `--include-secret` and redirect stdout to a
protected file. The CLI refuses `--include-secret` when stdout is a terminal.

A mode-`0600` owner ticket is saved under the aispace config directory. It contains the upload and
revoke capabilities and the recipient token. Treat it like a password; `transfer revoke` reads it
without placing a capability in process arguments.

If the process stops before finalization, rerun `aispace transfer resume <transfer-id>`. The CLI
re-hashes the original source paths recorded in the ticket, reads owner status, keeps only uploaded
parts whose size and SHA-256 still match, and uploads the remainder before replacing the encrypted
manifest and idempotently completing the transfer.

### `aispace transfer receive`

```sh
aispace transfer receive [--token-file PATH] [--output DIR] [--yes] [--overwrite]
```

With no source flag, the command prompts for a complete link or canonical token. It also accepts
`AISPACE_TRANSFER_TOKEN`; `--token-file` requires mode `0600`. Supplying the secret as an argument
is supported for convenience but is not recommended on shared machines because process arguments
may be visible to other users.

The encrypted manifest is authenticated and displayed before the server-side claim is created.
The sender is shown as `Unknown sender` unless a signed identity is present—encryption does
not prove who sent a transfer. After confirmation, ciphertext is streamed into mode-`0600`
`.partial` files, retried with HTTP ranges at authenticated chunk boundaries, and checked against
both chunk tags and whole-file SHA-256 hashes. Existing paths are rejected unless `--overwrite` is
explicit. Final names appear only after every file verifies; only then does the CLI submit the
verified commit that consumes a limited download.

```sh
# Safest interactive path: the secret is never a process argument.
aispace transfer receive

# Automation: secret is read from a protected file and prompting is disabled.
aispace transfer receive --token-file ./handoff.token --output ./received --yes
```

### `aispace transfer resume`, `status`, and `revoke`

```sh
aispace transfer resume <transfer-id> [--ticket PATH]
aispace transfer status <transfer-id> [--json]
aispace transfer revoke <transfer-id> [--ticket PATH]
```

`resume` and `status` use the bot key to inspect uploaded parts. `revoke` reads the
transfer-scoped revoke capability from the saved owner ticket. Revocation is idempotent.

### `aispace login`

```
aispace login --key ask_... [--url https://aispace.sh]
```

Validates the key with `GET /v1/whoami`, then writes the config file. Prints the key's name,
prefix and the account email.

```sh
$ aispace login --key ask_9fK2mQ1xAbCdEfGhIjKlMnOpQrStUvWx
logged in as key "research-bot" (ask_9fK2mQ1x) for luigi@example.com
saved /home/you/.config/aispace/config.json
```

The URL is only written to the config file when it differs from the default `https://aispace.sh`.

| Exit | When |
|---|---|
| 3 | key rejected |
| 2 | `--key` missing (and `AISPACE_KEY` unset), or the key does not start with `ask_` |

### `aispace upload`

```
aispace upload <path|-> [--name N] [--expires 7d] [--link] [--link-expires 1h]
                        [--max-downloads N] [--content-type T] [--sha256]
                        [--private | --shared]
                        [--encrypt] [--recipient age1...] [--identity-out PATH] [--json]
```

Uploads one file with `POST /v1/files` (raw streamed body). `-` reads stdin.

| Flag | Default | Notes |
|---|---|---|
| `--name N` | basename of `<path>`, or `stdin` for `-` | Sent as `X-File-Name` |
| `--expires D` | server default (7d) | File lifetime, `X-Expires-In`; max 30d |
| `--link` | off | After upload, also create a Pro public share link and print its URL |
| `--link-expires D` | server default (1h) | Link lifetime; **requires** `--link`, exit 2 without it |
| `--max-downloads N` | unlimited | Link download cap; **requires** `--link`, exit 2 without it |
| `--content-type T` | sniffed from extension, else `application/octet-stream` | Stored type |
| `--sha256` | off | Compute SHA-256 locally and send `X-SHA256` so R2 verifies the body |
| `--private` | account setting | Restrict this upload to the current key |
| `--shared` | account setting | Make this upload readable by every key on the account |
| `--encrypt` | off | Encrypt locally using age X25519; the service receives ciphertext only |
| `--recipient age1...` | generated one-time recipient | Encrypt to an identity already held by the recipient |
| `--identity-out PATH` | print once on stdout/JSON | Save a generated identity to a new mode-`0600` file; never overwrites |

Stdin is spooled to a temporary file first because the API requires `Content-Length`; the temp
file is removed after the request. There is no size limit on the CLI side; the server enforces
`max_file_bytes` (5 MB on Free, 100 MB on Pro) and returns exit 4 if exceeded. Check
`aispace quota --json | jq .limits.max_file_bytes` before large uploads.

When neither `--private` nor `--shared` is supplied, the server applies the account's “Share files
between my keys” setting, which is disabled by default. Account sharing is authenticated and does
not create a public URL. `--link` is a separate, explicit public-sharing action available on Pro.

```sh
# file
aispace upload ./report.pdf --expires 3d

# stdin from a pipeline
some-command | aispace upload - --name output.log

# upload + link in one go
echo "hello" | aispace upload - --name hello.txt --link --link-expires 1h --max-downloads 3

# end-to-end encrypted; keep secret.agekey and send it separately from the URL
aispace upload secret.pdf --encrypt --identity-out secret.agekey --link --max-downloads 1
```

Encrypted uploads are written to an owner-only temporary file first so their encrypted
`Content-Length` and SHA-256 are known. The stored name gains `.age`, the content type is
`application/vnd.aispace.age`, and the `File.enc_alg` field is `age-x25519`. If `--recipient` is
not supplied, the command generates a one-time identity. Losing that identity makes the file
unrecoverable. Anyone who has both ciphertext and the identity can decrypt it even after a link
is revoked, so deliver them through separate channels when practical.

Human output (one line per object). With `--link` the URL is printed **last, on its own line**, so
an agent can take the final line of stdout:

```
uploaded 01J8ZQ3V9N7X2K4M6P8R0T2W4Y hello.txt 6 B expires 2026-09-12T17:00:00Z
link 01J8ZQ5C1D3F5H7J9L1N3P5R7T expires 2026-09-05T18:00:00Z max_downloads 3
https://aispace.sh/d/K3f9Qm2xP7vL1nR8sT4wY6zA0bC5dE9fG3hJ7kM2nP4
```

Timestamps are RFC 3339 in UTC, and `max_downloads` is `unlimited` when no cap was set.

`--json` output: the `File` object; with `--link`, an envelope `{ "file": File, "link": ShareLink }`
so both IDs and the URL are available in one parse. Encrypted uploads always return an envelope
with an additional `encryption` object. It contains `algorithm`, `recipient`, `original_name`, and
either the one-time secret `identity` or `identity_file`. Treat `identity` as a credential.

```sh
url=$(echo "$report" | aispace upload - --name report.md --link --link-expires 2h --json | jq -r .link.url)
file_id=$(aispace upload big.zip --json | jq -r .id)
```

Exit codes: 4 on any quota/size rejection, 5 on rate limit (uploads are never retried), 3 on bad
key, 2 for a usage error such as `--link-expires` without `--link` or pointing at a directory. A
path that does not exist is exit 1.

If the upload succeeds but the link cannot be created, the file line (and, for `--encrypt`, the
encryption block) is still printed before the error, so the stored file is not lost.

If the server *rejects* the upload (quota, size, rate limit, bad key), a file written by
`--identity-out` is removed again: nothing was stored, so that identity decrypts nothing, and
leaving it behind would make retrying the same command fail with `identity file already exists`.
When the request fails without a response or returns a 5xx server error, the outcome is uncertain,
so the identity is kept and a warning names it — check `aispace ls` before deleting it, because the
file may have been stored. When no `--identity-out` was supplied, the CLI creates a mode-`0600`
recovery identity under the private aispace configuration directory (`recovery/` beside
`config.json`). It removes that recovery copy after a definite success or rejection, but keeps it
and prints its path after an uncertain outcome. If the process is terminated abruptly, inspect that
directory before removing a leftover key; it may be the only way to decrypt a stored upload.

### `aispace download`

```
aispace download <file_id> --output <path|-> [--verify] [--json]
```

Downloads a file owned by the current key or shared with the account by another key. This uses
key authentication and does not create or require a public link. Destination files are mode
`0600`, never overwritten, and removed if the transfer fails. Use `--output -` for raw stdout;
it cannot be combined with `--json`.

```sh
aispace download 01J8ZQ3V9N7X2K4M6P8R0T2W4Y --output report.pdf
aispace download 01J8ZQ3V9N7X2K4M6P8R0T2W4Y --output report.pdf --verify
```

`--verify` hashes the bytes as they are written and compares the result with the SHA-256 the API
records for every uploaded file, so a truncated or altered transfer is caught rather than trusted.
It reads the file's metadata first, which costs **one extra request** against the per-minute rate
limit. An incomplete or incompatible server response without a digest exits 1 with `no_checksum`
rather than reporting a check it did not perform.

On a mismatch the exit code is 1 with `checksum_mismatch`, and the output file is removed, so a
caller that ignores the exit code cannot pick up a corrupt file. With `--output -` the bytes have
already gone to stdout by the time the digest is known; the mismatch is still reported and the
exit code is still 1, but the caller has to discard what it read.

```sh
aispace download "$id" --output report.pdf --verify || echo "corrupt, nothing written"

# --json adds the digest that was verified
aispace download "$id" --output report.pdf --verify --json | jq -r .sha256
```

### `aispace keygen`

```
aispace keygen [--identity-out PATH] [--json]
```

Generates an age X25519 identity and public recipient entirely locally. It does not require an
aispace account or make a network request. Without `--identity-out`, the secret identity is printed
to stdout. With `--identity-out`, the identity is written to a new mode-`0600` file and never
printed; an existing file is not overwritten.

```sh
aispace keygen --identity-out receiver.agekey
# recipient age1...
# identity saved receiver.agekey

aispace keygen --identity-out receiver.agekey --json
# {"algorithm":"age-x25519","recipient":"age1...","identity_file":"receiver.agekey"}
```

Give the `age1...` recipient to a sender, who can encrypt for it with
`aispace upload --encrypt --recipient age1...`. Keep the `AGE-SECRET-KEY-...` identity private.

### `aispace decrypt`

```
aispace decrypt <path|url|-> --output <path|-> [--identity-file PATH] [--json]
```

Downloads if necessary and decrypts locally. The identity is read from `--identity-file`, or from
`AISPACE_AGE_IDENTITY`; there is deliberately no identity value flag, to keep secrets out of shell
history and process listings. Local `.age` input defaults to the same path without `.age` when
`--output` is omitted; any other local input falls back to `<input>.decrypted`, and the command
refuses to write over its own input. URL and stdin inputs require `--output`.

Output files use mode `0600`, are never overwritten, and are deleted if age authentication fails.
`--output -` streams plaintext to stdout and cannot be combined with `--json`.

```sh
aispace decrypt report.pdf.age --identity-file report.agekey --output report.pdf
AISPACE_AGE_IDENTITY='AGE-SECRET-KEY-...' aispace decrypt 'https://aispace.sh/d/...' -o report.pdf
```

### `aispace link`

```
aispace link <file_id> [--expires 1h] [--max-downloads N] [--json]
```

Creates a new share link for an existing file (`POST /v1/files/:id/links`). Multiple links per
file are fine; each has its own expiry, cap and counter, and can be revoked independently.

```sh
aispace link 01J8ZQ3V9N7X2K4M6P8R0T2W4Y --expires 24h --max-downloads 1
# → link 01J8ZQ5C1D3F5H7J9L1N3P5R7T expires 2026-09-06T17:00:00Z max_downloads 1
# → https://aispace.sh/d/...          (last line, so `| tail -n1` is the URL)

aispace link 01J8ZQ3V9N7X2K4M6P8R0T2W4Y --json | jq -r '.url, .expires_at'
```

Exit 1 with `not_found` if the file is unknown to this key or already expired.

### `aispace ls`

```
aispace ls [--limit N] [--cursor C] [--all] [--json]
```

Lists this key's live files (`GET /v1/files`). By default it follows `next_cursor` to the end, in
both human and `--json` mode. `--all` is accepted for compatibility and does nothing.

Human output is one line per file, `<id> <size> <expires> <name>`, with no header:

```
01J8ZQ3V9N7X2K4M6P8R0T2W4Y 1.0 MB 2026-09-12T17:00:00Z report.pdf
01J8ZQ5C1D3F5H7J9L1N3P5R7T 6 B 2026-09-12T17:01:00Z hello.txt
```

When the key holds no files, nothing is written to stdout and `no files` goes to stderr, so a
`--json`-free pipeline stays empty. `--json` prints every page merged into one object, with
`next_cursor` `null` because the walk is already finished:

```sh
# total bytes held by this key
aispace ls --json | jq '[.files[].size_bytes] | add'

# files expiring within 24 h
aispace ls --json | jq -r --argjson t "$(date +%s)" '.files[] | select(.expires_at - $t < 86400) | .name'
```

#### Paging a large account

Walking to the end costs one request per page, which is wasteful when only the first few files are
wanted. `--limit N` stops after N files instead:

```sh
aispace ls --limit 50 --json
```

Each request asks for exactly the number still wanted, so `next_cursor` stays aligned with what was
consumed — it is the resume point, not the end of the last page. Pass it back with `--cursor` to
continue, and it is `null` once the listing is exhausted:

```sh
cursor=""
while :; do
  page=$(aispace ls --limit 100 ${cursor:+--cursor "$cursor"} --json)
  echo "$page" | jq -r '.files[].id'
  cursor=$(echo "$page" | jq -r '.next_cursor // empty')
  [ -n "$cursor" ] || break
done
```

Resuming is exact: no file is skipped and none is returned twice. In human mode stdout stays one
line per file and the resume hint goes to stderr, so a pipeline is unaffected:

```
$ aispace ls --limit 3
01J8ZQ3V9N7X2K4M6P8R0T2W4Y 1.0 MB 2026-09-12T17:00:00Z report.pdf
...
more files remain; continue with --limit 3 --cursor 3        # stderr
```

### `aispace rm`

```
aispace rm <file_id> [<file_id>...] [--continue] [--json]
```

Deletes files (`DELETE /v1/files/:id`). All share links of the file stop working immediately and
the key's used bytes drop. Prints `deleted <id>` per file; with `--json`, one object **per line**
(JSON Lines, not one array):

```
{"deleted":"01J8ZQ3V9N7X2K4M6P8R0T2W4Y"}
{"deleted":"01J8ZQ5C1D3F5H7J9L1N3P5R7T"}
```

IDs are processed in order and the command **stops at the first failure**, so the IDs after it are
not deleted; the ones already printed were. The exit code is the one for the underlying error (1
for `not_found`, 3 for a bad key), and when more than one ID was given the message is prefixed
with the ID that failed.

`--continue` attempts every ID instead. Failures are reported to stderr as they happen, successes
still go to stdout, and the command exits non-zero if any ID failed — so a cleanup pass over a list
that may contain already-deleted or expired IDs completes in one call:

```sh
aispace ls --json | jq -r '.files[].id' | xargs aispace rm --continue
```

A failure that applies to every remaining ID still stops the run: a rejected key (`401`) or an
exhausted rate limit (`429`) would fail for all of them, so `--continue` gives up rather than
spending the rest of the batch on requests that cannot succeed. A missing ID is not treated that
way, because it says nothing about the others.

With `--json` the two streams stay separate: stdout carries only `{"deleted": id}` lines, and each
failure is a JSON error object on stderr, so a caller parsing stdout is never handed a mixed
stream.

### `aispace revoke`

```
aispace revoke <link_id> [<link_id>...] [--continue] [--json]
```

Revokes share links (`DELETE /v1/links/:id`). The file remains. Link IDs come from
`upload --link --json`, `link --json`, or `aispace links <file_id>`.

Like `rm`, it prints `revoked <id>` per link (or `{"revoked":"<id>"}` per line with `--json`),
processes IDs in order, stops at the first failure, and accepts `--continue` to attempt every ID
instead.

### `aispace links`

```
aispace links <file_id> [--json]
```

Lists the links of a file (`GET /v1/files/:id/links`) with `download_count`, `max_downloads`,
`expires_at`, `revoked_at`. URLs are not shown (the server does not keep them).

### `aispace info`

```
aispace info <file_id> [--json]
```

File metadata (`GET /v1/files/:id`).

### `aispace quota`

```
aispace quota [--json]
```

Five lines, one per group:

```
key: used 1.0 MB of 5.0 MB, 4.0 MB remaining
account: used 3.0 MB of 10.0 MB, 7.0 MB remaining, plan free (extra blocks 0)
month: uploads 12/100, downloads 340/1000, resets 2025-10-01T00:00:00Z
limits: max file 5.0 MB, max file ttl 7d, max link ttl 7d, uploads 60/h 500/d, requests 300/min
rate: uploads remaining 58 this hour, 490 today; requests remaining 299 this minute
```

The typed response is `key`, `account`, `month`, `limits` and `rate` — see
[`internal/api/types.go`](../internal/api/types.go). Monthly counters are account-wide and reset at
`month.period_end`. Exhaustion surfaces as `429 monthly_upload_cap` or
`429 monthly_download_cap` and is never retried automatically.

```sh
aispace quota --json | jq '.key.remaining_bytes'
aispace quota --json | jq -e '.rate.uploads_hour_remaining > 0' >/dev/null || echo "wait"
aispace quota --json | jq '.month | {uploads_left: (.uploads_limit - .uploads_used), downloads_left: (.downloads_limit - .downloads_used), period_end}'

# largest file this account may upload right now
aispace quota --json | jq '[.limits.max_file_bytes, .account.remaining_bytes] | min'
```

### `aispace doctor`

```
aispace doctor [--json]
```

Runs the checks that explain why a command is failing, in the order a command hits them: where the
key comes from, whether the server URL is usable, whether the key is accepted, and whether any
allowance is spent. Nothing is uploaded, nothing is modified, and the key is never printed.

```
$ aispace doctor
aispace 0.4.0 (darwin/arm64)

ok    config       key from AISPACE_KEY
      config       -> the config file is also set and is being ignored
ok    config-file  /home/you/.config/aispace/config.json
ok    url          https://aispace.sh
ok    auth         research-bot (ask_9fK2mQ1x) for you@example.com
warn  quota-key    3.0 MB of 50.0 MB remaining
      quota-key    -> delete files with `aispace rm`, or raise the key budget in the dashboard
fail  quota-uploads  monthly upload cap reached (100/100), resets 2026-10-01T00:00:00Z

1 check failing, 1 warning
```

Every check that can still run does run, so one problem does not hide another; checks that cannot
run report `skip` rather than being omitted.

**The exit code is the one the failing command would itself have returned**, so a caller branches on
the codes it already knows rather than a second set:

| First failing check | Exit |
|---|---|
| no key, rejected key, revoked key | `3` |
| unusable `--url`, or a key containing a control character | `2` |
| storage allowance exhausted | `4` |
| monthly upload or download cap reached | `5` |
| server unreachable, or any other failure | `1` |

Warnings never change the exit code, so `doctor` exiting `0` means "nothing is blocking a command
right now", not "nothing worth reading".

```sh
aispace doctor --json | jq -r '.checks[] | select(.status=="fail") | "\(.name): \(.detail)"'
aispace doctor >/dev/null || echo "not ready, exit $?"
```

### `aispace whoami`

Prints key name, prefix, account email and the key's usage against its budget
(`GET /v1/whoami`):

```
research-bot (ask_9fK2mQ1x) luigi@example.com used 1.0 MB of 50.0 MB
```

Exit 3 if the key is invalid, and also if the server reports the key as revoked.

### `aispace version`

```
aispace 0.3.1 (darwin/arm64)
```

The version string is injected at build time with `-ldflags "-X main.version=..."` and is `dev` in
an unstamped local build. `--json` prints the version and the exact `User-Agent` the client sends:

```json
{"user_agent":"aispace-cli/0.3.1 (darwin/arm64)","version":"0.3.1"}
```

## Recipes

## Human and device handoff

Every sealed bearer transfer has equivalent HTTPS, CLI-token, and native-link encodings. The
canonical CLI token embeds the HTTPS service origin and includes a transcription checksum:

```sh
# Preferred: protected prompt, mode-0600 file, or environment variable.
aispace handoff encode
aispace handoff encode --token-file ./handoff.token
AISPACE_TRANSFER_TOKEN='aispace-transfer-v1....' aispace handoff encode

# Convenience only; process arguments can be visible to other local users.
aispace handoff encode 'https://aispace.sh/t/01...#as1....'
```

The command prints secrets, so it is intentionally unavailable with `--json`. The native URI is a
local wrapper; opening it still requires its embedded HTTPS origin to match the configured
`AISPACE_URL` or `--url` value.

For a nearby browser or second CLI, create a five-minute, one-use pairing code from the mode-`0600`
owner ticket and keep the sender running:

```sh
aispace handoff offer 01...
# code    J7KM-PQRT
# open    https://aispace.sh/pair/J7KM-PQRT

aispace handoff receive J7KM-PQRT --output ./incoming
```

The receiver sees the exact service origin, transfer mode, sender status, size, file count and
expiry before approval. The short code never derives or returns the transfer key. After approval,
the sender encrypts the canonical intent to a one-use P-256 receiver key; the service relays only
that ciphertext. A second distinct receiver invalidates the room and both users must start again.

`handoff receive` applies the normal sealed-transfer preflight, authentication, partial-file and
verified-commit behavior. `--yes` skips only the handoff approval prompt; it does not weaken origin,
envelope, manifest, chunk, or file verification. Pairing and secret-bearing encoding are disabled
in JSON mode.

Built-in QR rendering and native OS scheme registration are not part of this experimental CLI
release; the printed HTTPS pairing URL is the no-install fallback.

## Agent identity and inbox commands

```text
aispace identity create --name NAME --handle SLUG
aispace identity list
aispace identity show ID
aispace identity disable ID
aispace identity rotate ID --purpose encryption|signing
aispace identity revoke ID KEY_ID

aispace recipient add INVITATION [--alias SLUG]
aispace recipient verify ALIAS --fingerprint FINGERPRINT
aispace recipient list
aispace recipient remove ALIAS

aispace transfer create PATH --sealed --to ALIAS [--from ID] [--also-link]
aispace inbox list [--cursor CURSOR] [--limit 100]
aispace inbox receive DELIVERY_ID [--identity ID] [--output DIR] [--yes]
                        [--overwrite] [--allow-unknown-sender]
aispace inbox reject DELIVERY_ID [--identity ID]
aispace inbox processed DELIVERY_ID [--identity ID]
```

`recipient remove` deletes the caller-owned server pin before deleting its
local trust entry. An already-absent server pin is treated as a successful
idempotent removal; a service error leaves the local entry intact for retry.

An unverified recipient is rejected unless the send supplies the complete
`--recipient-fingerprint` or explicitly opts into `--trust-on-first-use`.
Every send resolves the public record again and blocks a changed fingerprint.
Recognized rotations may advance both key purposes and span up to 32 historical
successors; every predecessor record, public-key byte string and signature must
form a complete chain back to the local pin. Any gap requires explicit repinning.
Addressed sends do not print a bearer decryption link unless `--also-link` is
present.

Invitation origins are pinned with recipient records, and local identities are
bound to the server where they were created. Identity, recipient, sender and
inbox key operations fail rather than reuse those records with another
configured origin. Legacy trust records without an origin must be re-imported.

Identity creation generates X25519 recipient and Ed25519 signing keys locally.
Private records live in `identities/` beside the CLI config and are atomically
written mode `0600`; only public keys and proof-of-possession signatures are
uploaded. Rotation retains old encryption private keys so already-addressed
deliveries remain readable. There is no server-side recovery.
Challenge, identity-publication, rotation and inbox-claim replay state is saved
locally with mode `0600` before the request. Re-running the same command recovers
the original server response and promotes the already-generated private key
instead of generating an incompatible replacement.

`inbox receive` unwraps the content key with RFC 9180 HPKE, checks manifest
recipient and sender bindings, verifies every authenticated chunk and file
hash, installs files only after verification, then submits recipient-signed `downloaded` and
`verified` receipts. A valid signature from a key that is not locally pinned is
reported as `Signature valid; sender unknown`, never as a verified sender.
Historical sender verification uses the authenticated claim-time key snapshot,
not a live public lookup. Revoked or expired signing keys and disabled sender
identities are shown explicitly and are never labeled verified.
`--yes` refuses such a sender (including an unsigned delivery) unless
`--allow-unknown-sender` is also supplied; interactive receive can still show
the manifest and ask the operator explicitly.

Exact signed downloaded, verified, processed and rejected receipt requests are
persisted before submission. A restart replays the same receipt ID, signature,
claim nonce and idempotency key until the service acknowledges it, then advances
or removes the local sequence state.

Upload a directory as a tarball with a single-download link:

```sh
tar czf - ./out | aispace upload - --name out.tgz --expires 1d --link --link-expires 6h --max-downloads 1
```

Script-safe upload with error handling:

```sh
if out=$(aispace upload result.csv --link --json 2>err.txt); then
  echo "share: $(jq -r .link.url <<<"$out")"
else
  case $? in
    4) echo "quota exceeded: $(cat err.txt)"; aispace quota ;;
    5) echo "rate limited: $(cat err.txt)"; sleep 60 ;;
    3) echo "bad key" ;;
    *) echo "failed: $(cat err.txt)" ;;
  esac
fi
```

Rotate to a new key without downtime: create the new key in the dashboard, run
`aispace login --key ask_new...` (config is overwritten atomically), then revoke the old key. Files
uploaded under the old key stay until their expiry and their links keep working; they are only
listable via the dashboard, not via `ls` under the new key.

Point at a local dev server:

```sh
AISPACE_URL=http://localhost:8787 AISPACE_KEY=ask_dev... aispace quota
```

## Shell completion

```
aispace completion <bash|zsh|fish|powershell>
aispace completion install [bash|zsh|fish] [--dir PATH] [--force] [--json]
```

`aispace completion <shell>` writes the script to stdout. `aispace completion install` writes it to
the directory that shell already reads, so nothing has to be sourced by hand.

### Persistent installation

```sh
aispace completion install            # shell taken from $SHELL
aispace completion install zsh        # or name it
```

The destination follows the XDG variables the rest of the CLI honours, and the path is printed:

| Shell | Destination |
|---|---|
| bash | `${XDG_DATA_HOME:-~/.local/share}/bash-completion/completions/aispace` |
| zsh | `${XDG_DATA_HOME:-~/.local/share}/zsh/site-functions/_aispace` |
| fish | `${XDG_CONFIG_HOME:-~/.config}/fish/completions/aispace.fish` |

An existing file is **never replaced**; the command exits `2` and names the file, so a hand-edited
completion is not silently lost. Pass `--force` to replace it, or `--dir` to install somewhere else
(for example a system-wide `/etc/bash_completion.d`). Only that one file is written.

Two shells need one more step, reported on stderr so stdout stays just the installed path:

- **zsh** reads the directory only if it is on `$fpath`. Add to `~/.zshrc` if missing:

  ```sh
  fpath=(~/.local/share/zsh/site-functions $fpath)
  ```

- **bash** reads the directory only when the `bash-completion` package is loaded.

Start a new shell afterwards, or re-run the shell's completion init.

### Current session only

Nothing is written to disk; the completions last until the shell exits.

```sh
source <(aispace completion bash)          # bash
source <(aispace completion zsh)           # zsh
aispace completion fish | source           # fish
```

### PowerShell

PowerShell loads completions from a profile rather than a directory, so `install` does not support
it and says so. Append the script to your profile instead:

```powershell
aispace completion powershell | Out-String | Invoke-Expression          # current session
aispace completion powershell >> $PROFILE                               # persistent
```
