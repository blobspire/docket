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
- **A short block in `~/.claude/CLAUDE.md`:** it tells every agent on the machine when to bring something to you in docket, and why the bar is high. `docket uninstall` removes it, and backups of the previous file are kept.
- **The Claude Code plugin:** hooks that deliver your answers and notice what an agent can't file itself (an expired login, a stuck lead). It also includes the `docket` skill, which agents read before filing.

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

Install the [ntfy](https://ntfy.sh) app and subscribe to the topic in your config (`init --server` picks a random one).

Pushes go out only for **act-now** items: an agent marked something `--urgency now`, or a Claude login failed. Each goes out once, 90 seconds after it became urgent, at any hour. Everything else waits on the dashboard.

Other programs can use the same path:
- `docket issue "<what broke>" --project <p> --new-project "<description>" --urgency now --key <id>` raises an alert. `--new-project` makes it work even before the project exists; once it does, it only fills in a missing description.
- `docket done --key <id> --project <p>` clears it.
- An alert that clears within the grace period never buzzes.

### Projects

Agents choose and create projects as they file. Seed the ones you know so agents reuse them:

```sh
docket projects add website --why "The company website and blog"
```

Map a lead to its project with `project = "website"` in its `[[lead]]`. Put docket's own health items (machines, logins, quota) where you like with `system_project = "admin"`. You can rename, merge or archive projects from a project's page on the dashboard.

### Ideas (dictation)

To dictate ideas from your phone and have agents research and build them, run a whisper.cpp server next to docket and add `[transcribe]` and `[ideas]` to the config. [ideas.md](ideas.md) has the steps; without a speech-to-text server, ideas can still be typed.

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
docket import md STATUS.md --project website --section "Human tasks"   # task lists you keep in markdown
```

## Upgrading

1. Install the new binary everywhere (`install.sh` again, or copy a `make dist` binary). Use a temporary file and `mv` it into place, because a running hook may hold the old one open.
2. On the server, run `systemctl --user restart docket`.
3. On every machine with agents:
   - Run `docket init --agent` to refresh the `CLAUDE.md` block.
   - Run `claude plugin marketplace update docket && claude plugin update docket@docket`.
4. Update laptops and lab machines before the server. A newer server sends them files (`context put`) that an older binary doesn't know.
5. New sessions pick up the new instructions; running sessions keep the old ones until they restart.

## Day to day

Open your docket at `https://<server>.<tailnet>.ts.net`; the docket mark at the top left always brings you back, and its badge counts what needs you (red when something needs action now).

- **Rows** have one-tap ✓ Done, 👎 Not worth my time and × Dismiss buttons: they show on hover with a mouse, and always on a phone, where you can also swipe a row right for done or left to dismiss.
- **Click a row** to open the item. The answer dock is pinned under it: the agent's options on top (the recommended one filled in), Reply beside them, and Done, Not worth it, Dismiss, Stash and Snooze in the same places on every item. After you act, the next item opens.
- **Answers waiting on agents** fold under each project until the agent closes the item, or for a day after it was delivered (then they're in Done). One whose delivery is stuck says so.
- **Stash** (`t`, or the toast after you answer or mark something done) keeps an item handy for 30 days without leaving it on your docket.
- **Most actions** can be undone from their toast (`u` too); Back to docket is its own undo. Press `?` for keys:

| Key | Action |
|---|---|
| `j` / `k` | next / previous item (in the list or an open item) |
| `enter` / `esc` | open the item / back to the list |
| `1`–`9` | pick one of the agent's options |
| `r` | reply in your own words |
| `d` / `x` | done / dismiss |
| `w` | not worth my time, with a note |
| `t` | stash |
| `s` / `S` | snooze until tomorrow / for an hour |
| `[` / `]` | switch project |
| `u` | undo |
| `o` | open the agent's session |

Answers reach the session right away: at its next tool call if it's working, by waking it if it's idle, or by resuming it if it ended.
