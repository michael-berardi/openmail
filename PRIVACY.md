# Privacy

OpenMail keeps your mail on your machine and talks only to the servers you configure.

- **Network:** your IMAP and SMTP servers; OAuth endpoints of Google or Microsoft if you sign in that way; DNS and the Thunderbird provider database when you add an account (to find server settings); and any remote images in a message only after you allow them.
- **No telemetry, no accounts, no cloud service.** Nothing is sent to the author.
- **Storage:** a SQLite database in your user data folder (mail, search index, drafts, rules, audit log) and secrets in the OS keychain.
- **Agents:** an MCP client sees only what its key's scope allows, and `agent.expose_bodies` can hide message bodies from it entirely.
