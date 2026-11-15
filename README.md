# OpenMail

A mail client for people who use both a browser and command-line tools, with scoped MCP access for agents. One Go binary stores IMAP mail in local SQLite and provides search, drafts, SMTP sending and a web app. MIT licensed.

## Install

**macOS app (Apple silicon and Intel):** download the signed, notarized [app zip from the latest release](https://github.com/michael-berardi/openmail/releases/latest), unzip it, and drag OpenMail to Applications.

**Command line:** download and extract your platform's CLI archive from the [latest release](https://github.com/michael-berardi/openmail/releases/latest), then put `openmail` (`openmail.exe` on Windows) on your `PATH`.

CLI binaries are available for macOS (universal), Linux (arm64 or x86-64), and Windows (x86-64). Each release includes `SHA256SUMS`. Linux and Windows use `openmail serve`; there is no desktop app for those platforms.

### From source

From a source checkout, with Go 1.26.4 or newer and Make:

```sh
make install
```

This builds the CLI and installs it in `~/.local/bin`, creating that directory if needed. Add it to your `PATH`, or run `~/.local/bin/openmail`. SQLite is compiled in. Password storage uses the OS keychain; Linux needs a running Secret Service.

To build the macOS desktop app, also install Xcode command-line tools and `rsvg-convert` (from librsvg), then run `make install-app`. This installs into `~/Applications`; a local build is not notarized.

## Try it

Use a new, separate directory for fictional mail:

```sh
openmail demo --home ./demo-mail --no-open
```

Open the printed local URL yourself. The demo does not start the sync engine. Stop it with Ctrl-C. Never point the demo at an existing mail data directory: it can display existing accounts.

For your own mailbox:

```sh
openmail accounts add you@example.com
openmail serve
```

Account setup discovers IMAP/SMTP settings and checks the login. Provider policies may require app passwords or [OAuth configuration](docs/oauth.md). `serve` opens a browser by default; use `--no-open` to print the URL without opening one. See [Apple Mail migration](docs/apple-mail.md) to import accounts, rules, signatures and VIPs.

## Mail and agents

- Local full-text search, conversations, IMAP IDLE, incremental CONDSTORE sync and queued offline changes.
- Server-synced drafts, formatted composition, snooze, rules and an undo-send window.
- Remote images blocked by default; sanitized message HTML in a sandboxed frame.
- Themes, density, text size, keyboard shortcuts and a command palette.
- Scoped MCP over stdio and HTTP, send approval and an audit log. See [agent setup and policy](docs/agents.md).

```sh
openmail ls 'from:sender subject:"q3 plan" is:unread has:attachment after:7d in:inbox budget'
openmail mcp --scope write
```

| Commands | Purpose |
|---|---|
| `ls [query]`, `read <id>`, `thread <id>` | Read mail; each accepts `--json` |
| `send`, `reply <id>`, `forward <id>`, `drafts …` | Compose and send |
| `archive`, `trash`, `junk`, `flag`, `mark`, `move`, `snooze` | Organise |
| `accounts …`, `folders …`, `sync` | Manage accounts and syncing |
| `rules …`, `config …`, `tokens …`, `audit` | Configure and inspect |
| `doctor` | Check the database, keychain and account connections |

Data defaults to `~/Library/Application Support/OpenMail` on macOS, `$XDG_DATA_HOME/openmail` (or `~/.local/share/openmail`) on Linux, and `%APPDATA%\OpenMail` on Windows. Override it with `OPENMAIL_HOME`.

The web app defaults to `127.0.0.1:7777`, requires a token or authenticated cookie, checks Host headers and protects cookie-authenticated writes. Agent scopes are `read`, `write`, `send` and `admin`; agents cannot approve their own sends. Message content is untrusted input, even after filtering.

IMAP and SMTP only: no Exchange EWS/ActiveSync, POP, calendar, contacts app, S/MIME or PGP.

## Development

```sh
make test  # go vet, race tests and isolated install regression test
make build # CLI in bin/openmail
```

The service layer is in `internal/core`; SQLite in `internal/store`; the API and embedded UI in `internal/web`; MCP in `internal/mcpserver`. See [Contributing](CONTRIBUTING.md), [Security](SECURITY.md) and [Privacy](PRIVACY.md).

---

Part of [Pareto Cybernetics](https://paretocybernetics.com/): free, private, open-source tools for working with AI agents.
