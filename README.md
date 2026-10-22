# FileSage

<p align="right"><img src="web/public/filesage.svg" width="64" alt="FileSage logo" /></p>

[![CI](https://github.com/int2t05/FileSage/actions/workflows/ci.yml/badge.svg)](https://github.com/int2t05/FileSage/actions/workflows/ci.yml)
[![MIT License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

A local-first file workspace for Linux. FileSage manages real directories across
mounted disks and lets you review AI-proposed file changes before they run.

[简体中文](README.zh-CN.md)

## Status

Early MVP development. There is no stable release or prebuilt package yet. See
the [acceptance scope](docs/ACCEPTANCE.md) for verified capabilities and
remaining checks.

## What it does

- Browse, search, filter, upload and organize files in their native directory
  structure. Mount additional disks beneath the configured root; folders remain
  the only categories.
- Resume interrupted uploads (20 GiB per file and 20 GiB total staging by
  default; both configurable), download original files, and create or inspect
  supported archives.
- Read common image, audio, video, PDF, text, Markdown, Office, spreadsheet,
  EPUB and archive formats in the browser. Unsupported or complex files remain
  available for download.
- Ask an OpenAI-compatible model to clarify a task, then review the complete
  plan before it runs. Continue with follow-up tasks in the same saved
  conversation, including after a refresh. `bypasspermission` is on by default
  and runs steps in order; turn it off in Agent settings to run one step at a
  time. Expand a step to inspect its command, exit code, bounded output and
  file diff, then check the audit history. Bounded Shell output is retained in
  the task record and can inform the next turn in the same conversation.
- Keep SQLite task and search metadata outside the managed files. The filesystem remains the source of truth.

## Quick start

Requirements: Linux, Go 1.27.3 or newer, Node.js 24 and npm. Optional system
tools enable formats and operations such as Office conversion, OCR, video
posters and 7-Zip archives; see [deployment](docs/DEPLOYMENT.md).

```sh
cd web
npm ci
npm run build
cd ..
go build -o ./filesage ./cmd/filesage

mkdir -p "$HOME/FileSageFiles" "$HOME/.local/state/filesage"
export FILESAGE_ROOT="$HOME/FileSageFiles"
export FILESAGE_DATA_DIR="$HOME/.local/state/filesage"
export FILESAGE_WEB_DIR="$(pwd)/web/dist"
export FILESAGE_LISTEN_ADDR="127.0.0.1:8080"
./filesage
```

Open `http://127.0.0.1:8080`. `FILESAGE_ROOT` must already exist and
`FILESAGE_DATA_DIR` must be outside it. The default listener is loopback. For
LAN access, bind only to a trusted private interface and configure the host
firewall. FileSage has no login or password.

## Verify a change

```sh
go test -count=1 ./...
go vet ./...
go build ./...

cd web
npm ci
npm test
npm run typecheck
npm run build
npx playwright install chromium
npm run e2e
```

The browser test uses a local mock model and temporary files/database.
[GitHub Actions](.github/workflows/ci.yml) also runs race detection,
vulnerability scanning and a Linux package smoke test.

## Project docs

- [Deployment and configuration](docs/DEPLOYMENT.md)
- [Product requirements](docs/PRD.md) · [technical decisions and ADRs](docs/TECH.md)
- [UI/UX](docs/design/UIUX.md) · [icon gallery](docs/design/icon-study.html) · [interactive prototype](docs/design/prototype/README.md)
- [Feature acceptance and format support](docs/ACCEPTANCE.md)
- [Project progress and remaining work](docs/IMPLEMENTATION_PLAN.md)
- [Competitor and implementation research](reference/README.md)
- [Contributing](CONTRIBUTING.md) · [security reporting](SECURITY.md)

## Support and feedback

Report a reproducible bug or suggest an improvement through
[GitHub Issues](https://github.com/int2t05/FileSage/issues/new/choose). Do not
post secrets, private file contents or exploitable security details there; see
the [security reporting guide](SECURITY.md) for private reports.

## Security boundary

FileSage is intended for a trusted personal network. It does not authenticate
users, provide HTTPS termination or protect an internet-facing deployment. Keep
it on loopback or a trusted LAN behind a firewall. Agent chat messages are sent
to the configured model as you converse. File-analysis tools read and send file
contents only after you confirm an analysis card; review its listed data types
first. Shell does not read file bodies automatically. After you confirm a Shell
run, stdout and stderr are stored locally (up to 16 KiB per stream). The next
turn sends the confirmed command, scope, working directory, execution status,
bounded output and changed paths, types and statuses to the configured model
as untrusted evidence. Command output may contain file contents or other sensitive information. See the
[Agent API notes](docs/agent-conversation-api.md).

## License

FileSage-authored code and documentation are licensed under the
[MIT License](LICENSE). Third-party assets and dependencies retain their own
terms; see [third-party notices](THIRD_PARTY_NOTICES.md). The public repository
contains research summaries, source links and collection tools; locally
collected upstream documents are excluded.
