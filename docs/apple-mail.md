# Moving from Apple Mail

```
openmail import apple --dry-run   # show what was found
openmail import apple             # import, asking for passwords it cannot find
```

## What is read

| From | Imported as |
|---|---|
| macOS Internet Accounts (`~/Library/Accounts/Accounts4.sqlite`) | accounts, with Apple's own server settings when present, otherwise discovered |
| `~/Library/Mail/V*/MailData/SyncedRules.plist` (and Unsynced) | rules (rules that use unsupported conditions are listed as skipped) |
| `MailData/Signatures` | account signatures (set only where the account has none) |
| `MailData/VIPSenders.plist` | VIPs |

Nothing in Apple Mail is modified. Your mail itself is downloaded again from the servers, so IMAP accounts end up complete; Apple Mail can keep running alongside OpenMail.

## Permission

macOS protects those folders. Give your terminal **Full Disk Access** (System Settings → Privacy & Security → Full Disk Access), or copy `~/Library/Accounts` and `~/Library/Mail` into a folder shaped like a home directory and pass `--from <folder>`.

## Passwords

macOS keeps the passwords of accounts set up in Internet Accounts in a store other apps cannot read, so OpenMail cannot copy them. It asks the Keychain anyway (for accounts saved the old way), then asks you:

- Zoho, Fastmail, Yahoo, iCloud: create an **app password** in the provider's security settings.
- Gmail: an app password (myaccount.google.com/apppasswords) or OAuth sign-in ([oauth.md](oauth.md)).
- Microsoft: OAuth sign-in or an app password if your organisation allows them.

Exchange (EWS/ActiveSync) accounts are not IMAP and are reported as such; use the IMAP settings of the mail host if it offers them.

Imported rules start **off**: Apple Mail keeps running its copy, and two clients applying the same copy or move rule would duplicate mail. Turn them on once you stop using Mail for that account.

## Notes

- Apple stores Google and Microsoft sign-ins as protected tokens that other apps cannot read; those accounts sign in again.
- Smart mailboxes and Mail's "On My Mac" folders are not imported.
