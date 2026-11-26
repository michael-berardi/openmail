# Privacy

OpenMail keeps your mail on your machine and talks only to the servers you configure.

- **Network:** your IMAP and SMTP servers; OAuth endpoints of Google or Microsoft if you sign in that way; DNS and the Thunderbird provider database when you add an account (to find server settings); and any remote images in a message only after you allow them.
- **No telemetry, no accounts, no cloud service.** Nothing is sent to the author.
- **Storage:** a SQLite database in your user data folder (mail, search index, drafts, rules, audit log capped at 1,000 entries) and secrets in the OS keychain. The folder is 0700 and database files are 0600. Account passwords and OAuth tokens are not written to the database (agent tokens are stored as hashes); an OAuth client secret you configure is kept in the local settings table.
- **Deleting it:** stop OpenMail, run `openmail accounts remove <address>` for each account to clear keychain credentials, then delete the data folder (`OPENMAIL_HOME` overrides its default location).
- **Agents:** an MCP client sees only what its key's scope allows, and `agent.expose_bodies` can hide message bodies from it entirely.
