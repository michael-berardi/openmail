<!-- update-contract: v1 -->
# Updates

OpenMail tells you when a newer stable release is published and installs it in one step. Automatic installation is off until you turn it on.

## The check

- One anonymous GET to `https://api.github.com/repos/michael-berardi/openmail/releases/latest` with the User-Agent `openmail-update-check`. It sends no version, identifier, machine or usage data.
- At most once every 24 hours per machine, shared by the CLI and the app. It runs in the background and never delays a command or the app's start.
- Only stable `x.y.z` releases newer than yours count. Drafts, prereleases and other tags are ignored.
- A failed check is recorded with its cause and shown by `openmail update --check`. It is not retried until the next day.
- Automatic checks are off in CI, in agent shells, for `openmail mcp`, for `--json` output, when output is not a terminal, with `OPENMAIL_NO_UPDATE_CHECK=1`, and when Settings → Updates → "Check for updates automatically" is off (`openmail config set update.check false`).

## Command line

After a command finishes, an interactive terminal shows, at most once a day per version:

```text
openmail 0.3.0 available (you have 0.2.0) · run: openmail update
turn on automatic updates: openmail update --auto on · skip: openmail update --skip
```

```sh
openmail update                  # show the notes, ask "Install 0.3.0 now? [Y/n]", install
openmail update --check          # exit 0 up to date, 10 update available, 1 check failed
openmail update --skip           # no notice for this version; a newer one notifies again
openmail update --auto on|off|status
openmail update --rollback       # restore the previous executable
```

`openmail update` without a terminal installs without asking. After the first install on a terminal it asks once, "Install future updates automatically? [y/N]".

The installer downloads your platform's CLI archive from the release, checks its SHA256 against the release's `SHA256SUMS` before writing anything, then swaps the executable and keeps one rollback copy, `<executable>.previous`. If a step fails, the current version keeps working and no downloaded files remain. The rollback copy is removed after the new version completes a command successfully, or after seven days. If Homebrew installed OpenMail, the notice and `openmail update` use `brew upgrade openmail` instead.

With automatic updates on, the daily check installs a new release in a detached, low-priority process; it applies on the next run. Results go to `update.log` (at most 256 KB). If an automatic install fails, the next command shows the normal notice. Package-manager installs are never updated automatically.

## macOS app

When an update is available, a prompt in the corner of the window shows "OpenMail 0.3.0 is available — you have 0.2.0.", the release notes, **Skip This Version**, **Remind Me Later**, **Install Update**, and an unchecked **Automatically download and install updates in the future** box. Settings → Updates has "Check for updates automatically" (on), "Install updates automatically" (off), the current version and **Check Now**.

Install Update downloads `OpenMail-v<version>-macos-universal.zip`, checks it against `SHA256SUMS`, and requires a valid code signature, a Gatekeeper (notarization) pass and the same Developer ID Team ID as the running app before swapping `OpenMail.app` and keeping `OpenMail.app.previous`. OpenMail reopens only while no message is being written; automatic installs also wait for a minute without typing or clicking. The rollback copy is removed the next time the new version opens.

## Files and removal

The update cache is one file, `update.json`, in `~/Library/Caches/openmail` (macOS), `$XDG_CACHE_HOME/openmail` or `~/.cache/openmail` (Linux), or `%LocalAppData%\openmail` (Windows). `openmail update --auto off` deletes `update.log`. To uninstall, delete the app or executable and that folder.

For testing, `OPENMAIL_UPDATE_API` points the check at another release record and `OPENMAIL_CACHE_DIR` moves the cache.
