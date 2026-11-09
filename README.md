# docket

**One queue for everything your agents need from you.**

> Status: **pre-alpha**, built in the open and used daily by its author. Capture, the queue, phone pushes and answer delivery work (milestones M0–M1). Leads, the chief of staff's health checks and preference learning are in progress. Expect rough edges.

If you run several coding agents across projects and machines, the things only you can do get buried:
- an approval an agent is holding
- a permission prompt
- a login that expired
- "you'll need to plug in the drive"
- an issue it mentioned once, 400 messages ago, before a context compaction

You find out days later, when you ask "do you need anything from me?".

docket gathers those into **one prioritized queue** with the evidence attached (screenshots, videos, reports). It sends a phone push only when something is truly blocked, and delivers your answer back to the agent right away so the work continues.

## How it works

```
 each machine (laptop, servers, lab box)         one always-on server
 Claude Code ─hooks─▶ docket hook ─┐             docket serve
 agent ──────────▶ docket ask ─────┼─▶ spool ◀──ssh pull── collector ─▶ SQLite ─▶ web queue
                                   │                                          └─▶ phone push
 answers ◀──────────── mailbox / tmux ◀──────── deliver ◀── you decide
```

- **Capture, with no extra model tokens:**
  - Agents file items with a non-blocking CLI (`docket ask`, `docket task`, `docket review`, …).
  - Claude Code hooks catch what agents don't file: permission prompts, auto-mode refusals, "needs input" notifications, login and API failures.
  - A cheap regex self-check nudges an agent that asked for something in chat without filing it.
- **Pull, not push:** each machine appends to a local spool, and the server pulls over SSH. Machines never need to reach the server, which suits laptops that sleep and locked-down lab machines.
- **One binary:** server, CLI, hooks and an embedded web UI, with SQLite and no runtime dependencies.
- **Claude Code first,** behind an adapter interface, so other agent CLIs can be added.

The full design and roadmap are in [docs/design.md](docs/design.md).

## Set it up

On **every machine with agents**:

```sh
git clone https://github.com/blobspire/docket && cd docket
make install                                   # ~/.local/bin/docket (Go 1.27+; or copy a `make dist` binary)
docket init --agent --machine laptop --person Sam   # also adds docket's block to ~/.claude/CLAUDE.md
claude plugin marketplace add blobspire/docket
claude plugin install docket@docket
```

On the **always-on server** (one of those machines):

```sh
mkdir -p ~/.config/docket && cp docs/examples/docket.example.toml ~/.config/docket/docket.toml   # edit it
cp deploy/systemd/docket.service ~/.config/systemd/user/ && systemctl --user enable --now docket
sudo tailscale serve --bg --https=443 http://127.0.0.1:7788     # private HTTPS on your tailnet
docket server test-push
```

Laptops that sleep run `docket sync --server <server>` (see `deploy/launchd/`). Machines the server can SSH into need nothing else.

To start with what's already waiting: `docket backfill grep --since 30d` on each machine, and `docket import md <file> --section "<heading>"` for task lists kept in markdown.

## License

MIT
