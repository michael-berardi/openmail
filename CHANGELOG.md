# Changelog

## 0.2.0 (2026-11-26)

- One-line CLI installers for macOS/Linux (`install.sh`) and Windows (`install.ps1`): exact asset selection, SHA-256 verification against `SHA256SUMS`, versioned install folders and PATH shims. They refuse to replace files they did not install.
- On macOS, `install.sh --app` verifies and installs the release's OpenMail.app into `~/Applications`.
- Release assets now include both installers and their checksums.
- OpenMail tells you when a new release is out: after a command in the terminal, and with an in-app prompt in the macOS app. The check is one anonymous request at most once a day and stays silent in CI, agent shells, MCP, `--json` output and pipes.
- New `openmail update` installs the release after checking it against `SHA256SUMS`, keeps one rollback copy, and uses `brew upgrade` for Homebrew installs. `--check`, `--skip`, `--auto on|off|status` and `--rollback` cover the rest.
- The macOS app installs updates only after verifying the checksum, code signature, notarization and Developer ID Team ID, and reopens only while no message is being written.
- Automatic installation is opt-in: Settings → Updates or `openmail update --auto on`. See [Updates](docs/updates.md).
- Privacy: remote images and CSS URLs stay blocked by default; no telemetry, analytics or accounts were added. With this release OpenMail does make one anonymous daily update check, which you can turn off with `OPENMAIL_NO_UPDATE_CHECK=1` or Settings → Updates.
- The local audit log is capped at 1,000 entries, and the data directory plus SQLite database, WAL and SHM files are created with private permissions.
- README now has a short Privacy section, with the full detail in [PRIVACY.md](PRIVACY.md): what leaves the machine, what is stored, and how to delete local mail.


## 0.1.1 (2026-11-18)

- Mail shortcuts no longer act on a hidden conversation while Settings, Rules, Accounts, Agents or Drafts is open.
- Discarding a draft with content, deleting a draft or a rule, and revoking an agent key now ask first.
- Settings shows "Dark message bodies" and "Compose with formatting" as on when they are on.
- Apple Mail import no longer fails when the Mac has no rules or VIPs.
- Attach is reachable from the keyboard. Sidebar counts are announced with the right noun.
- Phones: the compose buttons wrap instead of running off screen, the closed menu leaves the tab order, Escape closes it, and New message closes it.
- Switching folders shows "Loading…" instead of the previous folder's messages.
- Messages inside a dialog appear at the top, clear of Send and Save.
- Counts read "1 draft", "2 drafts" instead of "draft(s)".
- Adding an account stops checking for sign-in once the dialog is closed.

## 0.1.0 (2026-10-19)

- Incremental sync with CONDSTORE, messages over 25 MB downloaded on demand, compressed local storage.
- Drafts mirror to the server's Drafts folder; server drafts can be taken over for editing.
- Formatted (rich text) composer.
- macOS desktop app (`make install-app`): native window, Dock badge, notifications, mailto: links, menus.
- `openmail import apple --diagnose` for bug reports.

- IMAP sync with IDLE push, offline change queue, full-text search, conversation threading, snooze, rules, VIPs.
- Add an account from an address: provider list, Mozilla autoconfig, DNS SRV, MX hints, host probe. Passwords in the OS keychain; Google and Microsoft OAuth with your own client IDs.
- Import Apple Mail accounts, rules, signatures and VIPs (`openmail import apple`).
- Web app: three columns, command palette, remappable shortcuts, themes, density, text size, high-contrast theme, keyboard and screen reader support.
- MCP server (stdio and HTTP) with scopes, a send policy, approval flow, prompt-injection hardening and an audit log.
- CLI for every action, with JSON output.
