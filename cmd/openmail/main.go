// Command openmail is an open, scriptable, agent-ready mail client.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `OpenMail — open mail for people and agents

Usage: openmail <command> [options]

Start here
  serve                     run OpenMail: sync, web app, API and MCP over HTTP
  accounts add <email>      add an account (servers are discovered)
  import apple              bring over Apple Mail accounts, rules, signatures, VIPs

Mail
  ls [query]                list messages or conversations (--threads, --json)
  read <id>                 show a message
  thread <thread-id>        show a conversation
  send                      send a message (--to --subject --body)
  reply <id>                reply to a message (--all, --body, --send)
  drafts <list|show|send|approve|cancel|delete>
  mark | archive | trash | junk | flag | move | snooze <ids...>
  sync [account]            fetch new mail now (--full, --older N)
  unsubscribe <id>          use the sender's unsubscribe link

Setup
  accounts <list|add|remove|test|login|password|update>
  rules <list|add|rm|run>
  folders [account]         list folders; folders sync <account> <folder> on|off
  config <list|get|set>
  tokens <create|list|revoke>    keys for agents and API clients
  mcp                       MCP server on stdio (--scope read|write|send|admin)
  audit                     what the user, rules and agents did
  doctor                    check setup and fix hints
  version

Search syntax: from:alice to:bob subject:"q3 plan" is:unread|flagged|vip has:attachment
               after:7d before:2026-01-31 in:inbox account:me@x.com label:tag  free words
Data lives in ~/Library/Application Support/OpenMail (override with OPENMAIL_HOME).
`

type cmdFunc func(ctx context.Context, args []string) error

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	cmds := map[string]cmdFunc{
		"serve": cmdServe, "demo": cmdDemo, "mcp": cmdMCP, "accounts": cmdAccounts, "account": cmdAccounts, "import": cmdImport,
		"ls": cmdList, "list": cmdList, "search": cmdList, "read": cmdRead, "thread": cmdThread,
		"send": cmdSend, "reply": cmdReply, "forward": cmdForward, "drafts": cmdDrafts,
		"mark": cmdAct("read"), "unread": cmdAct("unread"), "archive": cmdAct("archive"), "trash": cmdAct("trash"),
		"junk": cmdAct("junk"), "flag": cmdAct("flag"), "unflag": cmdAct("unflag"), "move": cmdMove, "snooze": cmdSnooze,
		"sync": cmdSync, "unsubscribe": cmdUnsub, "rules": cmdRules, "config": cmdConfig, "tokens": cmdTokens,
		"audit": cmdAudit, "folders": cmdFolders, "doctor": cmdDoctor, "version": cmdVersion,
	}
	fn, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "openmail: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := fn(ctx, os.Args[2:]); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "openmail: %v\n", err)
		os.Exit(1)
	}
}
