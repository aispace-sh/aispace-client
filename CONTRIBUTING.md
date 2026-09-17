# Contributing to aispace

Thank you for helping make secure temporary file sharing easier to automate. Bug reports, focused
feature proposals, documentation fixes, tests, and portability improvements are welcome.

## Before opening an issue

- Search existing issues and the [`ROADMAP.md`](ROADMAP.md). Maintainers can publish the prepared
  [`good first issue` drafts](docs/GOOD_FIRST_ISSUES.md) as capacity allows.
- Use GitHub Security Advisories—not a public issue—for vulnerabilities or exposed credentials.
- Remove API keys, private links, file IDs, and encryption identities from logs and screenshots.
- Include `aispace version`, operating system, architecture, installation method, and the smallest
  reproducible command when reporting a bug.

## Local development

The Go and npm tests use local HTTP fixtures and do not require an aispace account or production
credentials.

```sh
git clone https://github.com/aispace-sh/aispace-client.git
cd aispace-client
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
(cd npm && npm test && npm pack --dry-run)
```

Build a local binary with:

```sh
go build -ldflags "-X main.version=0.0.0-dev" -o aispace .
./aispace version
```

The parsers that accept input from outside — sealed fragments and references,
delivery manifests, identity invitations, fingerprints, public keys, and handoff
intents — have fuzz targets. Their seed corpora run as ordinary tests, so
`go test ./...` already covers them. To search for new inputs:

```sh
go test ./internal/handoff/ -run xxx -fuzz FuzzParseOriginIsEnforced -fuzztime 2m
```

A failure writes the offending input to `testdata/fuzz/`; commit it with the fix
so it becomes a permanent regression case.

Tests that exercise the hosted service must use a dedicated low-budget key. Never commit a key or
place it in a fixture, command transcript, issue, or pull-request description.

Every `README.md` or Markdown document under `docs/` must be listed in `docs/index.json`, which
groups pages for documentation consumers. JSON files are treated as metadata unless explicitly
listed as API-reference documents. Run `node scripts/validate-index-json.mjs .` after adding,
moving, or removing a document; CI rejects missing, duplicate, orphaned, or dead entries.

## Pull requests

Keep each pull request focused on one behavior. Add or update tests for behavior changes and update
the CLI/API documentation when a public flag, response, error, or exit code changes. Before opening
the pull request, run the same checks listed above.

Commit messages should use a concise Conventional Commit subject, such as:

```text
feat: add resumable downloads
fix: preserve exit code on link failure
docs: add CI report example
```

By contributing, you agree that your contribution is licensed under the repository's MIT license.
