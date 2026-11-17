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

## Transport

- **The server pulls** from machines over the system `ssh` (your keys and `~/.ssh/config`). Laptops push with `docket sync` over `ssh` to the server. Nothing listens for inbound connections on agent machines.
- **The remote commands are fixed** (`spool export|ack|blob`, `mailbox put`, `sessions`, `session send|start|resume`, `quota`, `usage`, `rules put`), and their arguments are validated: session ids, pane ids and file names have strict formats.

## Reporting a vulnerability

Please open a private security advisory on GitHub (Security → Report a vulnerability) rather than a public issue.
