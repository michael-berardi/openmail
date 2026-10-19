# Security

Report vulnerabilities privately through GitHub's "Report a vulnerability" (Security tab) rather than a public issue. Expect an acknowledgement within a few days.

## Model in brief

- The interface and API listen on `127.0.0.1` only, require a bearer token, reject unexpected `Host` headers, and require a custom header on cookie-authenticated writes.
- Passwords and OAuth refresh tokens live in the OS keychain, never in the database or logs.
- Message HTML is sanitised and rendered in a sandboxed frame with a `default-src 'none'` policy; remote content is blocked until allowed.
- MCP and API keys have scopes (`read`, `write`, `send`, `admin`). Agents cannot approve their own sends, change `agent.*` settings, or create rules that delete mail.
- Every state change is audited with its actor.

See [README.md](README.md#security-model) and [docs/agents.md](docs/agents.md).
