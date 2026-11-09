# Setting up docket

docket runs as a **server** on one always-on machine, plus a small **agent side** on every machine where Claude Code runs. The server can be one of those machines too.

```
laptop (sleeps)  ──docket sync──▶  server (always on)  ◀──ssh──  lab box (can't reach the server)
```

## 1. Install the binary everywhere

```sh
curl -fsSL https://raw.githubusercontent.com/blobspire/docket/main/scripts/install.sh | sh
```

This puts a static binary at `~/.local/bin/docket` (no sudo; Linux and macOS, amd64 and arm64). You can also build it from a clone with `make install` (Go 1.27+), or copy a `make dist` binary to machines that can't download.

## 2. On every machine with agents

```sh
docket init --agent --machine <name> --person <your name>
claude plugin marketplace add blobspire/docket
claude plugin install docket@docket
```

What this sets up:
- **`~/.docket/`:** the local spool the server collects, the mailbox answers arrive in, and the approved preferences.
- **A short block in `~/.claude/CLAUDE.md`:** it tells every agent on the machine to file what it needs from you with `docket`. `docket uninstall` removes it, and backups of the previous file are kept.
- **The Claude Code plugin:** hooks that capture permission prompts, auto-mode refusals, "needs input" notifications and login errors, and that deliver your answers. It also includes the `docket` skill.

Sessions already running pick up the plugin when they restart. Until then the server can still see them through Claude Code's session registry, and it reaches the ones in tmux by typing into them, guarded so it never types while you're attached or mid-sentence.

Optional: if you want usage limits shown, set the status line to docket's. It can wrap a status line you already have:

```json
"statusLine": {"type": "command", "command": "~/.local/bin/docket statusline -- <your existing command>"}
```

## 3. On the server

```sh
docket init --server --person <you> --public-url https://<server>.<tailnet>.ts.net --install-service
loginctl enable-linger "$USER"                                  # keep running when logged out
sudo tailscale serve --bg --https=443 http://127.0.0.1:7788     # private HTTPS on your tailnet
docket server test-push
```

Edit `~/.config/docket/docket.toml` to list your other machines. The fields are documented in [examples/docket.example.toml](examples/docket.example.toml).

| Machine | Config | What else it needs |
|---|---|---|
| The server itself | `mode = "local"` | nothing |
| Reachable over SSH (desktop, lab box) | `mode = "pull"`, `ssh = "<alias>"` | `ssh <alias>` must work non-interactively from the server |
| A laptop that sleeps or travels | `mode = "push"` | run `docket sync --server <server>` every 30 s (see `deploy/launchd/`) |

Restart the service after editing the config: `systemctl --user restart docket`.

### Phone pushes

Install the [ntfy](https://ntfy.sh) app and subscribe to the topic in your config (`init --server` picks a random one). Pushes go out only when:
- a lead is blocked on you,
- a system is down, or
- something is due today.

Quiet hours hold everything except system-down alerts. Everything else waits in the queue.

## 4. Leads (optional)

A lead is a long-running agent responsible for one area. Add one per area:

```toml
[[lead]]
name = "website"
machine = "laptop"
dir = "~/code/site"
match_dir = true          # sessions working in this folder count as the lead
priority = 2              # ranks its items higher
schedule = ["daily@09:30"]
wake_on = ["job_stale", "job_done"]
```

Leads are event-driven: your answers, their jobs, or a schedule wake them. Schedules run inside the server and survive reboots. Put a `LEAD.md` in the repo describing the lead's scope, what it may decide alone, and what it must bring to you, and import it from the project's `CLAUDE.md` with `@LEAD.md`.

## 5. Start with what is already waiting

```sh
docket backfill grep --since 30d          # on each machine: asks already in transcripts become "maybe" items
docket import md STATUS.md --section "Human tasks"   # task lists you keep in markdown
docket server backfill-prompt --dry-run   # then without --dry-run: live sessions file what they're waiting on
```

## Day to day

Open the queue at `https://<server>.<tailnet>.ts.net`. Press `?` for keys: `j`/`k` to move, `y`/`n` to approve or decline, `a` to answer, `o` to open the agent's session. Answers reach the session right away: at its next tool call if it's working, by waking it if it's idle, or by resuming it if it ended.
