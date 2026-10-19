# Google and Microsoft sign-in

OpenMail ships no OAuth client IDs; you register your own, once. Until then Gmail and Outlook work with app passwords.

## Google

1. Google Cloud Console → create a project → enable the Gmail API.
2. OAuth consent screen: External, add yourself as a test user (or publish).
3. Credentials → Create OAuth client ID → **Desktop app**.
4. `openmail config set oauth.google.client_id <id>` and `openmail config set oauth.google.client_secret <secret>`.
5. `openmail accounts add you@gmail.com` and sign in in the browser.

Scope requested: `https://mail.google.com/`.

## Microsoft

1. Entra admin center → App registrations → New registration, any account type, redirect URI **Mobile and desktop** `http://127.0.0.1`.
2. API permissions: `IMAP.AccessAsUser.All`, `SMTP.Send`, `offline_access` (Office 365 Exchange Online).
3. `openmail config set oauth.microsoft.client_id <application id>`.
4. `openmail accounts add you@outlook.com`.

Refresh tokens are stored in the OS keychain.
