# docket

**One queue for everything your agents need from you.**

> Status: **pre-alpha**, built in the open. Right now docket captures events on each machine (milestone M0). The queue, phone alerts and answer delivery are next. Don't depend on it yet.

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

## Try the capture layer (M0)

```sh
git clone https://github.com/blobspire/docket && cd docket
make install                       # builds ~/.local/bin/docket (needs Go 1.27+)
docket init --agent --machine laptop
claude plugin marketplace add blobspire/docket
claude plugin install docket@docket
# use Claude Code as usual, then:
docket spool tail -n 5
```

Nothing leaves the machine yet: events go to `~/.docket/spool/`.

## License

MIT
