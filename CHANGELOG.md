# Changelog

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
