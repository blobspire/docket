# Email triage: docket reads your inbox, drafts your replies, and digests the rest

Three times a day docket reads the mail accounts you connect and sorts what's new:

- **Email · needs you.** Each email you should answer becomes an item at the bottom of your docket, with a reply already drafted. An agent wrote it in your voice after checking the facts: the whole thread, your earlier mail with that person, and, when the email is about one of your projects, that project's code. You can:
  - **Send** it. It goes out after an Undo window (30 seconds by default), from your own account, threaded into the conversation, with your signature.
  - **Ask for changes** in your own words ("shorter, and mention Friday"). The agent revises it and the new version replaces the old one.
  - **Edit** it yourself.
  - **Save to drafts**, to finish it in your mail app.
  - **Send a holding reply** ("good idea, we'll add it this week"), when the real answer needs work first.
  - Close it without replying (Don't reply, Not worth it, Dismiss), or Stash or Snooze it like any item.
- **Email · worth reading.** One line per email that matters but needs no reply: a meeting moved, a deadline, a result posted. Each line is written so you don't have to open the email ("Lab meeting moved Thu → Fri 2pm"). Lines you've scrolled past fold into Earlier when the next check brings new ones, tick one off with **Read**, and they fade after three days, so there's nothing to clear. Tap a line to open the whole email, with a link to it in Gmail or Outlook.
- **Skipped.** Everything else: newsletters, receipts, notifications. The Email header links to what the last check skipped, so you can see what it left out.

If you've already answered an email from your phone or laptop, its reply item closes on the next check.

**Work an email asks for.** When answering well needs work in one of your projects first (say, a neighbor asks for a feature on the site you run), the drafting agent checks the code. If the feature doesn't exist, it writes a plan, and docket files it as an ordinary proposal in that project's section, linked to the reply both ways. The draft is written as the reply to send once it's done, with a holding reply to send now. Tap **Build it** and a session builds, tests, merges and releases it; when it ships, the reply is revised to say so and comes back to the top of Email for you to send. Save it for later or dismiss it, and the reply is revised to match. Only people you've written to before can put work in front of you this way.

**It learns.** "Needs a reply" on a digest line or a skipped email, and "Not worth my time" on a reply or a digest line, are remembered, and every later check sees your recent corrections. (They stay out of the rules your coding agents read.)

**Email never shouts.** Email items never push your phone and never appear in Act now or the docket's badge count. The Email sections show their own count.

**Safety.** Email is written by strangers, and some of it is written to fool an AI reading your inbox. docket is built so that a fooled agent still can't do harm: agents never touch your mailbox, they can only read a single folder, drafts are stripped of anything that could hide text, and only your tap sends, only what you saw, only to the people in the thread. See [SECURITY.md](../SECURITY.md#email).

## Set it up

Mail is read and sent by the docket server, the always-on machine. Each account needs IMAP and SMTP access and a password in a file only you can read. Passwords never go in `docket.toml`.

### Gmail

1. Turn on 2-Step Verification for your Google account (an app password needs it).
2. Create an app password at <https://myaccount.google.com/apppasswords> (name it "docket").
3. Save it on the server without it showing on screen or in your shell history:

   ```sh
   umask 077; mkdir -p ~/.config/docket/secrets
   printf "App password: "; stty -echo; IFS= read -r p; stty echo; echo
   printf %s "$p" | tr -d " " > ~/.config/docket/secrets/gmail-app-password; unset p
   ```

4. Add the account to `~/.config/docket/docket.toml`:

   ```toml
   [email]

   [[email.account]]
   name = "gmail"
   label = "Gmail"
   address = "you@gmail.com"
   display_name = "Your Name"
   imap = "imap.gmail.com:993"
   smtp = "smtp.gmail.com:587"
   password_file = "~/.config/docket/secrets/gmail-app-password"
   ```

   Gmail's Promotions and Social tabs are skipped before any model sees them (`skip = ""` turns that off, or set your own Gmail search).

### Microsoft 365 / Outlook, through DavMail

Many organizations don't allow third-party apps to read mail. [DavMail](https://davmail.sourceforge.net) is a gateway that signs in to Microsoft 365 like Outlook does and serves IMAP and SMTP on the server's loopback.

1. Install Java (`sudo apt install openjdk-21-jre-headless`) and unzip DavMail's platform-independent package into `~/.local/opt/davmail`.
2. Configure it (`~/.config/davmail/davmail.properties`) to listen only on loopback, in server mode, with device-code sign-in:

   ```properties
   davmail.server=true
   davmail.mode=O365Graph
   davmail.authentication=O365DeviceCode
   davmail.bindAddress=127.0.0.1
   davmail.allowRemote=false
   davmail.imapPort=1143
   davmail.smtpPort=1025
   davmail.caldavPort=
   davmail.ldapPort=
   davmail.popPort=
   davmail.oauth.persistToken=true
   davmail.oauth.tokenFilePath=/home/you/.config/davmail/davmail.tokens
   davmail.userWhiteList=you@example.edu
   davmail.smtpSaveInSent=true
   ```

   If your organization blocks DavMail's own app, its documentation describes using Outlook's client ID instead (`davmail.oauth.clientId`, with `davmail.enableOidc=false`).
3. Run it as a service whose output goes to a file docket can read, so docket can show you a sign-in code when one is needed (`StandardOutput=append:/home/you/.local/state/davmail/davmail.out.log` in its systemd unit).
4. Pick a local password for the gateway (any random string; DavMail encrypts its Microsoft token with it) and save it in `~/.config/docket/secrets/davmail-password` (mode 600).
5. Add the account:

   ```toml
   [[email.account]]
   name = "work"
   label = "Work"
   address = "you@example.edu"
   display_name = "Your Name"
   imap = "127.0.0.1:1143"
   smtp = "127.0.0.1:1025"
   security = "plain"                # only allowed for a gateway on this machine
   password_file = "~/.config/docket/secrets/davmail-password"
   signature_html = "~/.config/docket/signatures/work/signature.html"   # optional; images beside it go inline
   signature_text = "~/.config/docket/signatures/work/signature.txt"
   signin_log = "~/.local/state/davmail/davmail.out.log"
   web = "outlook"
   ```

6. The first check signs in: Microsoft wants a code entered at <https://login.microsoft.com/device>. docket files an item with the code; it lasts about 15 minutes, and **Check now** under Email gets a fresh one. After that DavMail renews its token itself.

### Any other mailbox

Any IMAP and SMTP server works (Fastmail, iCloud with an app-specific password, your own server). Use its host names and ports; TLS is required (implicit on 993 and 465, STARTTLS otherwise).

### Check it

```sh
docket server email-check          # logs in to every account, reads and sends nothing
docket server email-check --read   # also reads new mail into docket (prints counts only)
```

Then restart the server (`systemctl --user restart docket`). The first check runs at the next scheduled time; **Check now** under Email runs one right away.

## Options

All under `[email]` in `docket.toml`:

| Option | Default | What it does |
|---|---|---|
| `schedule` | `["daily@08:00", "daily@12:30", "daily@17:30"]` | When checks run (in `timezone`); a check missed while the server was down runs once when it's back |
| `triage_model` | `claude-sonnet-5-5` | Sorts mail |
| `draft_model` | `claude-opus-5-5` | Researches and drafts replies |
| `max_running` | 4 | Drafts written at once. Every email that needs a reply gets one; the rest wait their turn |
| `voice` | `~/.claude/skills/draft-email/SKILL.md` | Your writing guide, read fresh for every draft. Any Markdown file describing how you write works |
| `digest_days` | 3 | How long a digest line stays |
| `keep_days` | 30 | How long message bodies are kept (headers stay, for threading) |
| `send_hold` | `30s` | The Undo window before a reply goes out |
| `backfill` | `3d` | How far back the very first check looks |
| `max_per_pass` | 300 | Messages read per folder per check (the newest) |

Per account (`[[email.account]]`): `folders` (default `["INBOX"]`), `sent_folder` and `drafts_folder` (found automatically), `aliases` (other addresses that are you), `skip` (a Gmail search never triaged), `signature_html`, `signature_text`, `signin_log`, `web` (`gmail` or `outlook`, for the Open in links), `label`.

## How it works

1. **Read.** Each check opens the configured folders and Sent read-only and fetches new messages by UID, newest first, in bounded batches. Threads come from `References` and `In-Reply-To`. The first check also reads a year of your Sent envelopes (addresses only) to learn who you've written to.
2. **Answered already?** A thread whose newest message is yours closes its reply item.
3. **Sort.** New threads go to the triage model in batches, with your projects, your recent corrections, and for each thread who sent it (and whether you've written to them), who it went to, list and bulk markers, and the text. It returns reply, digest or skip and a one-line summary.
4. **Draft.** Each reply gets a drafting session ([SECURITY.md](../SECURITY.md#email) lists what it can and can't do). docket cleans what comes back, chooses the recipients from the thread, and stores each version. Revisions resume the same session.
5. **Send.** Your tap holds the shown version for the Undo window, then docket builds the message (styled HTML and a matching plain-text part, your signature, the quoted original, threading headers) and sends it over the account's SMTP. Gmail and DavMail put it in your Sent folder.
