# Privacy

OpenMail keeps your mail on your machine and talks only to the servers you configure.

- **Network:** your IMAP and SMTP servers; OAuth endpoints of Google or Microsoft if you sign in that way; DNS and the Thunderbird provider database when you add an account (to find server settings); and any remote images in a message only after you allow them.
- **Update checks:** one anonymous GET to GitHub's public latest-release record for OpenMail, at most once every 24 hours, with the fixed User-Agent `openmail-update-check` and no version, identifier, machine or usage data. Automatic checks never run in CI, agent shells, MCP sessions or non-interactive output, and turn off with `OPENMAIL_NO_UPDATE_CHECK=1` or Settings → Updates. Release files are downloaded only when you install an update or turn on automatic installation.
- **No telemetry, no accounts, no cloud service.** Nothing is sent to the author.
- **Storage:** a SQLite database in your user data folder (mail, search index, drafts, rules, audit log), secrets in the OS keychain, and in the platform cache folder one private update file (`update.json`: last check time, latest version, notes, skipped version) plus a log of at most 256 KB while automatic updates are on.
- **Agents:** an MCP client sees only what its key's scope allows, and `agent.expose_bodies` can hide message bodies from it entirely.
