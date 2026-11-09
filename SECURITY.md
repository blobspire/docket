# Security model

docket relays decisions between you and agents that can run commands on your machines, so it is an approval channel. Read this before deploying it.

## Who can reach the queue

- **Network:** the server listens on `127.0.0.1` only. Expose it with `tailscale serve`, so only devices on your tailnet can reach it, over HTTPS.
- **Login allowlist:** set `allow_logins` to the Tailscale logins that may use it. Requests without an allowed `Tailscale-User-Login` header are refused.
- **Cross-site requests:** state-changing requests are checked against `Origin` and `Sec-Fetch-Site`.
- **Don't expose it publicly.** There is no password login. The tailnet is the boundary.

## What an answer can do

- **docket only relays your decision.** An answer goes to the session that filed the item, labeled as coming from the docket queue. docket never answers, approves or retries anything on its own, and that includes the chief of staff.
- **Auto mode still decides.** An approval for an action auto mode refused is delivered as your message; Claude Code's own permission checks then decide whether the retry runs. docket never tells the model to bypass them.
- **Physical actions are never approvable remotely.** That means items marked `--physical`, or matching a machine's `physical_keywords` such as robot motion. Deliveries to such machines also state that they don't authorize physical motion.
- **Every answer is logged** with its delivery method and receipt.

## What docket reads and stores

- **On each agent machine**, hooks append Claude Code's hook payloads to `~/.docket/spool/` (mode 0700): session ids, folders, prompts, the assistant's last message, and the commands of tool calls that needed permission. Long strings are truncated. Environment variables whose names look like secrets (`TOKEN`, `KEY`, `SECRET`, `AUTH`, …) are never recorded.
- **The server** copies those records into its SQLite database under `~/.local/share/docket/` (mode 0700). Evidence files agents attach are copied there too.
- **Agent-made HTML or SVG evidence** is served with `Content-Security-Policy: sandbox` and `nosniff`, so it can't run scripts in the queue's origin.
- **No model reads transcripts.** The optional chief-of-staff jobs (pre-review and rule proposals) send item text and your logged corrections to Claude through the server's own `claude` login. Turn them off with `[chief] pre_review = false` and `rules_every = "0"`.

## Transport

- **The server pulls** from machines over the system `ssh` (your keys and `~/.ssh/config`). Laptops push with `docket sync` over `ssh` to the server. Nothing listens for inbound connections on agent machines.
- **The remote commands are fixed** (`spool export|ack|blob`, `mailbox put`, `sessions`, `session send|start|resume`, `quota`, `usage`, `rules put`), and their arguments are validated: session ids, pane ids and file names have strict formats.

## Reporting a vulnerability

Please open a private security advisory on GitHub (Security → Report a vulnerability) rather than a public issue.
