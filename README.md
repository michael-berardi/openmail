# OpenMail

An open mail client for people and agents. One Go binary: sync engine, web app, command line and MCP server over a local SQLite database. MIT licensed.

## Download

Version 0.1.0. Each file is listed with its checksums in [SHA256SUMS](https://github.com/michael-berardi/openmail/releases/download/v0.1.0/SHA256SUMS). All releases are on the [releases page](https://github.com/michael-berardi/openmail/releases/latest).

| Platform | File |
|---|---|
| macOS app (Apple silicon and Intel) | [OpenMail-v0.1.0-macos-universal.zip](https://github.com/michael-berardi/openmail/releases/download/v0.1.0/OpenMail-v0.1.0-macos-universal.zip) |
| macOS command line (Apple silicon and Intel) | [openmail-cli-v0.1.0-macos-universal.zip](https://github.com/michael-berardi/openmail/releases/download/v0.1.0/openmail-cli-v0.1.0-macos-universal.zip) |
| Linux command line, arm64 | [openmail-cli-v0.1.0-linux-arm64.tar.gz](https://github.com/michael-berardi/openmail/releases/download/v0.1.0/openmail-cli-v0.1.0-linux-arm64.tar.gz) |
| Linux command line, x86-64 | [openmail-cli-v0.1.0-linux-amd64.tar.gz](https://github.com/michael-berardi/openmail/releases/download/v0.1.0/openmail-cli-v0.1.0-linux-amd64.tar.gz) |
| Windows command line, x86-64 | [openmail-cli-v0.1.0-windows-amd64.zip](https://github.com/michael-berardi/openmail/releases/download/v0.1.0/openmail-cli-v0.1.0-windows-amd64.zip) |

Linux and Windows have no desktop app; use `openmail serve` and open the printed address in a browser.

```
make install-app           # or download the notarized app from Releases
openmail import apple      # bring over your Apple Mail accounts, rules, signatures, VIPs
claude mcp add openmail -- openmail mcp --scope write
```

The desktop app is a Go program in a native window using the system web view, so there is no bundled browser: about 18 MB on disk and about 45 MB of memory in the app process. It syncs while open, keeps syncing with the window closed (quit with ⌘Q), shows the unread count on the Dock icon, posts notifications for new mail, handles `mailto:` links, and remembers its window size. `openmail serve` runs the same thing headless in a browser.

## What it does

- **Any IMAP/SMTP account.** Type an address; servers come from a provider list, Mozilla autoconfig, DNS SRV records, MX hints or a host probe. The login is checked against the real servers before anything is saved. iCloud, Gmail, Outlook, Yahoo, Fastmail, Zoho, GMX, Proton Bridge and custom domains work out of the box.
- **Apple Mail migration.** Reads the accounts, rules, signatures and VIPs Apple Mail already has, and looks for saved passwords in the macOS Keychain. See [docs/apple-mail.md](docs/apple-mail.md).
- **Fast, light and offline.** Local copy of your mail, IMAP IDLE push, incremental CONDSTORE sync, compressed storage, messages over 25 MB downloaded on demand, full-text search, conversations, snooze, undo send. Changes made offline are queued and replayed when the server is reachable; one that keeps failing is reported in the audit log.
- **Search syntax.** `from:alice subject:"q3 plan" is:unread has:attachment after:7d in:inbox account:me@x.com budget`
- **Customisable.** Themes (system, light, dark, high contrast), density, layout, text size, accent colour, every keyboard shortcut, a command palette (⌘K), rules.
- **Accessible.** Landmarks, listbox and dialog semantics, live announcements, full keyboard operation, visible focus, reduced motion, forced-colors support, text size up to 180%.
- **Private by default.** Remote images are blocked until you allow them. Message HTML is sanitised and shown in a sandboxed frame with a locked-down content policy. Passwords live in the OS keychain.
- **Built for agents.** An MCP server with scopes, a send policy, an approval flow and an audit log. See [docs/agents.md](docs/agents.md).

## Install

**macOS app:** download `OpenMail-<version>-macos-universal.zip` from the [latest release](https://github.com/michael-berardi/openmail/releases/latest), unzip, and drag OpenMail to Applications. It is signed and notarized, for Apple silicon and Intel.

**Command line (macOS, Linux, Windows):** download `openmail-cli-<version>-<os>-<arch>` from the same page, or build it:

```
git clone https://github.com/michael-berardi/openmail && cd openmail
make install        # builds bin/openmail and copies it to ~/.local/bin
openmail doctor     # checks setup and tells you how to fix anything
```

The command line and the MCP server share the app's data, so install both if you use agents. Building needs Go 1.26 (the macOS app also needs Xcode's command line tools). SQLite is compiled in; there are no other runtime dependencies. `SHA256SUMS` on each release lists the file checksums.

## Commands

| | |
|---|---|
| `serve` | sync engine, web app, JSON API and MCP over HTTP |
| `accounts add <email>` | add an account (`list`, `remove`, `test`, `login`, `password`, `update`) |
| `import apple` | import from Apple Mail |
| `ls [query]`, `read <id>`, `thread <id>` | read mail (`--json` on all) |
| `send`, `reply <id>`, `forward <id>`, `drafts …` | write mail |
| `archive`, `trash`, `junk`, `flag`, `mark`, `move`, `snooze` | organise |
| `rules …`, `config …`, `tokens …`, `audit` | configure |
| `mcp` | MCP server on stdio |
| `doctor` | diagnose |
| `demo --home DIR` | try the app on fictional mail, no network |

Data lives in `~/Library/Application Support/OpenMail` (macOS), `~/.local/share/openmail` (Linux), `%APPDATA%\OpenMail` (Windows). Set `OPENMAIL_HOME` to move it.

## Security model

- The web app and API listen on `127.0.0.1`, require a bearer token (or the cookie set by the printed URL), reject unexpected `Host` headers and require a header on cookie-authenticated writes.
- Tokens have scopes: `read`, `write`, `send`, `admin`. Only a person with the browser token can approve an agent's send.
- Message bodies shown to agents have hidden text, zero-width and bidi characters removed, are labelled untrusted, and are checked for spoofing and instruction-injection signals.
- Everything that changes state is written to an audit log.

## Scope

- Drafts, including reply and forward drafts, are saved to the server's Drafts folder shortly after you stop typing (replacing the previous version), so they follow you to Apple Mail and your phone. Drafts started elsewhere open with **Edit draft**.
- The composer switches between plain text and formatted text (bold, italic, lists, quotes, links). Pasted text is inserted as plain text.
- IMAP and SMTP only. Exchange (EWS/ActiveSync) and POP are out of scope, as are a calendar, contacts app, S/MIME and PGP.
- If Apple Mail import fails on your Mac, run `openmail import apple --diagnose` and share the output; it contains no addresses or names.

## Development

```
make test           # go vet and go test -race
make demo           # fictional mailbox on :7788
```

Layout: `internal/core` (service layer used by every surface), `internal/store` (SQLite), `internal/web` (API and embedded UI), `internal/mcpserver`, `internal/apple`, `internal/discover`, `internal/imapx`, `internal/compose`, `cmd/openmail`.

---

Part of [Pareto Cybernetics](https://paretocybernetics.com/): free, private, open-source tools for working with AI agents.
