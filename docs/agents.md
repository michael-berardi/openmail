# Agents and MCP

OpenMail speaks the Model Context Protocol over stdio and HTTP.

## Connect

Claude Code:

```
claude mcp add openmail -- openmail mcp --scope write
```

Claude Desktop, Cursor and other clients (stdio):

```json
{ "mcpServers": { "openmail": { "command": "openmail", "args": ["mcp", "--scope", "write"] } } }
```

HTTP (needs `openmail serve` running):

```
openmail tokens create claude --scope write
claude mcp add --transport http openmail http://127.0.0.1:7777/mcp --header "Authorization: Bearer <token>"
```

`openmail mcp` also syncs new mail while connected unless `--no-sync` is given. If `openmail serve` is already syncing, it defers to it.

## Scopes

| Scope | Tools |
|---|---|
| `read` | `get_overview`, `search_messages`, `list_threads`, `read_message`, `read_thread`, `save_attachment`, `search_contacts`, `list_drafts`, `list_rules`, `get_settings` |
| `write` | adds `create_draft`, `reply_to_message`, `forward_message`, `delete_draft`, `update_messages` (read, flag, archive, trash, junk, move, keyword, snooze), `sync_now` |
| `send` | adds `send_draft`, `unsubscribe` |
| `admin` | adds `create_rule`, `delete_rule`, `run_rules`, `set_setting`, `add_account`, `remove_account`, `audit_log` |

A client only sees the tools its scope allows. Prompts: `triage_inbox`, `draft_reply`, `summarize_thread`.

## Send policy

`openmail config set agent.send_policy <value>`

| Value | Behaviour |
|---|---|
| `draft_only` | agents can write drafts; `send_draft` is refused |
| `approve` (default) | `send_draft` holds the draft as *pending approval*; a person approves it in the app (Drafts) or with `openmail drafts approve <id>` |
| `allow` | queued with the undo window, subject to `agent.max_sends_hour` and `agent.allowed_domains` |

Agents cannot change `agent.*` settings, cannot approve their own sends, and cannot create rules that delete mail. Editing a draft clears its approval.

## Untrusted content

Email is attacker-controlled input. Tool results that contain message content carry a `notice`, hidden text (CSS-hidden, zero-size, tag-block and zero-width characters) is removed, and `security_notes` flags failed SPF/DKIM/DMARC, a differing Reply-To, and text aimed at AI assistants. `agent.expose_bodies=false` hides bodies from agents entirely.

## Audit

Every call is recorded with the client name. `openmail audit --actor mcp:` or the Agents page shows them.
