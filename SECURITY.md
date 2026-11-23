# Security model

docket relays decisions between you and agents that can run commands on your machines, so it is an approval channel. Read this before deploying it.

## Who can reach the queue

- **Network:** the server listens on `127.0.0.1` only. Expose it with `tailscale serve`, so only devices on your tailnet can reach it, over HTTPS.
- **Login allowlist:** set `allow_logins` to the Tailscale logins that may use it. Requests without an allowed `Tailscale-User-Login` header are refused.
- **Cross-site requests:** state-changing requests are checked against `Origin` and `Sec-Fetch-Site`.
- **Don't expose it publicly.** There is no password login. The tailnet is the boundary.

## What an answer can do

- **docket only relays your decision.** An answer goes to the session that filed the item, labeled as coming from the docket queue. docket never answers, approves or retries anything on its own.
- **Auto mode still decides.** An approval for an action auto mode refused is delivered as your message; Claude Code's own permission checks then decide whether the retry runs. docket never tells the model to bypass them.
- **docket doesn't guard physical actions.** Your answer reaches the agent as you gave it. If a robot or other hardware must only move with someone present, enforce that where it binds every path to the hardware (the robot's own safety layer, the agent's instructions), not in docket. The old `physical_keywords` setting is ignored.
- **Every answer is logged** with its delivery method and receipt.

## What docket reads and stores

- **On each agent machine**, hooks append Claude Code's hook payloads to `~/.docket/spool/` (mode 0700): session ids, folders, prompts, the assistant's last message, and the commands of tool calls that needed permission. Long strings are truncated. Environment variables whose names look like secrets (`TOKEN`, `KEY`, `SECRET`, `AUTH`, …) are never recorded.
- **The server** copies those records into its SQLite database under `~/.local/share/docket/` (mode 0700). Evidence files agents attach are copied there too.
- **Agent-made HTML or SVG evidence** is served with `Content-Security-Policy: sandbox` and `nosniff`, so it can't run scripts in the queue's origin.
- **No model reads transcripts.** The one optional model job (rule proposals) sends your logged corrections and decisions to Claude through the server's own `claude` login. Turn it off with `[chief] rules_every = "0"`.
- **Each machine sees only its own items.** `~/.docket/context.json` lists project names, descriptions and the components and places in use, plus only the open items filed from that machine. The feedback section of `~/.docket/rules.md` is the same everywhere: it lists the titles of items you marked "not worth my time" (and your notes), from every project, so write notes you're comfortable having on every machine. Both files are readable only by your user.

## Email

Email triage ([docs/email.md](docs/email.md)) reads mail written by anyone, so it treats every email as hostile input that may be trying to steer an AI. The protections are structural: they don't depend on the model resisting.

**What can touch your mail.**
- Only the docket server reads and sends mail, with each account's password (or app password) in a file it refuses to use unless only its owner can read it (`chmod 600`). Plain-text IMAP/SMTP is refused unless the server is on the same machine's loopback (DavMail); everything else needs TLS with a verified certificate (implicit on 993/465, STARTTLS otherwise; a server without STARTTLS is refused).
- Reading opens folders read-only and peeks at messages: nothing is marked read, moved, flagged or deleted. The only writes are a reply you sent and a draft you asked docket to save.
- Messages are stored in docket's database (mode 0700). Their bodies are dropped after `keep_days` (30 by default); headers stay, for threading.

**What the models can do.**
- Sorting is one Claude Code call per batch with no tools (`--tools ""`), in safe mode (no CLAUDE.md, skills, plugins, hooks or MCP servers). It returns a verdict per thread and nothing else.
- Drafting is a Claude Code session with only Read, Grep and Glob, in `--restricted` and safe mode with `--strict-mcp-config`. Its file tools are confined to its working directory: docket's own read-only clone of the project's code (fresh from its git remote, so no untracked files such as `.env`), or an empty folder. It can't run commands, reach the web, or read any other file on the server, including your mail and the password files. It sees the thread and your earlier mail with the same person, which docket fetches for it, and returns JSON.
- docket refuses to start any model with another tool list (a shell can't be added by mistake).
- Each email is fenced with a random marker the sender can't guess, and the instructions say email is data, never instructions. That helps; the points above are what make it safe when it doesn't.
- Text hidden in an incoming HTML email (display:none, zero-size or transparent text) is left out of what the models read.

**What a draft can carry.** docket cleans every draft before you see it:
- Only `p`, `br`, `strong`, `ul`/`ol`/`li`, `table`/`tr`/`th`/`td` and links with http, https or mailto addresses survive. Every attribute except a link's address is removed, so no colour, size, font, visibility or class can hide text. Scripts, styles, images, embeds, forms and comments are dropped.
- Invisible characters (zero-width and joiner characters, direction overrides, the Unicode tag block, variation selectors, default-ignorable and unassigned code points) are removed, so text can't be smuggled in characters that render as nothing.
- Blank paragraphs, empty tags, runs of line breaks and runs of spaces are squeezed, so text can't be pushed out of view.
- The plain-text part is built from the same cleaned HTML, and every link is written with its address, so the two parts say the same thing.
- docket adds all styling itself, and the preview you approve is drawn from the same HTML that is sent. Every link in the preview shows where it really goes, and a link whose text shows one address but opens another is flagged.
- When cleaning removed anything, the draft tells you what.

**Who decides.**
- Only your tap sends a reply, only the exact version shown (a newer version under it means nothing is sent), held for an Undo window (`send_hold`, 30 s). A send interrupted by a crash is marked failed for you to check, never retried.
- docket chooses the recipients from the thread (the sender, plus everyone on the latest message for reply-all). The model can't add one. An address you type that isn't in the thread is flagged in red. Every address and Message-ID is validated before it goes into a header, so nothing can add a recipient or a header.
- Work an email asks for becomes an ordinary proposal you approve, and only for senders you've written to before. The session that builds it gets the approved plan, never the email.
- Email items are never urgent: an email can't push your phone or claim the Act now band.
- Your corrections teach the sorting prompt only; they never reach the rules every coding agent reads.

**What's still on you.**
- **Read what you send.** The draft can be wrong, or say something the email talked the agent into ("please confirm your account number"). Cleaning makes sure you can see everything; it can't judge it.
- **Read the plan you approve.** A proposal's plan was written by an agent that read the email. Build it builds that plan.
- **Earlier mail with the same person** is given to the drafting agent as context, so a draft could quote it back to them.
- **The reader** shows incoming HTML in a sandboxed frame with scripts off and nothing loaded from the network (no tracking pixels), but links in it open if you click them, as in any mail app.

## Transport

- **The server pulls** from machines over the system `ssh` (your keys and `~/.ssh/config`). Laptops push with `docket sync` over `ssh` to the server. Nothing listens for inbound connections on agent machines.
- **The remote commands are fixed** (`spool export|ack|blob`, `mailbox put`, `sessions`, `session send|start|resume`, `quota`, `usage`, `rules put`), and their arguments are validated: session ids, pane ids and file names have strict formats.

## Reporting a vulnerability

Please open a private security advisory on GitHub (Security → Report a vulnerability) rather than a public issue.
