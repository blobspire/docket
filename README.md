# docket

**One queue for everything your agents need from you.**

> Status: **pre-alpha**, built in the open and used daily by its author across a laptop, a home server and a lab machine. It works, but expect rough edges and changes.

If you run several coding agents across projects and machines, the things only you can do get buried:
- an approval an agent is holding
- a permission prompt
- an expired login
- "you'll need to plug in the drive"
- an issue it mentioned once, 400 messages ago, before a context compaction

You find out days later, when you ask "do you need anything from me?".

docket collects those into **one prioritized queue** with the evidence attached. It pushes to your phone only when something is truly blocked, and it delivers your answer back to the agent right away so the work continues.

## What it does

- **Captures what agents need from you, with no extra model tokens.**
  - Agents file items with a non-blocking CLI: `docket ask`, `task`, `review`, `issue`.
  - Claude Code hooks catch what they don't file: permission prompts, auto-mode refusals, "needs input", login and API failures.
  - A cheap regex self-check nudges an agent that asked for something in chat without filing it.
- **Ranks the queue,** with a morning brief, a list of things only you can do, search over every past decision, and an evidence gallery of screenshots, videos and reports.
- **Delivers your answer immediately:**
  - into a busy session at its next tool call,
  - by waking an idle session,
  - by resuming a session that has ended,
  - or as soon as a sleeping laptop reconnects.
- **Pushes to your phone (ntfy) only when it matters:** a lead is blocked, a system is down, or something is due today.
- **Leads:** one long-running agent per area, woken by your answers, job events or schedules that survive reboots.
- **Job liveness:** a job that stops sending heartbeats becomes an alert.
- **Chief of staff:**
  - a watchdog for unreachable machines, sessions stuck on a dialog, expired logins and stalled jobs
  - usage limits per account, and which lead is spending
  - optional pre-review that annotates items and sends incomplete ones back
  - it never answers anything for you
- **Brain:** your corrections become proposed preference rules. You approve each one, and approved rules reach every agent.
- **Robot-safe:** physical actions can never be approved remotely.

## How it works

```
 each machine (laptop, servers, lab box)          one always-on server
 Claude Code ─hooks─▶ docket hook ─┐              docket serve
 agent ──────────▶ docket ask ─────┼─▶ spool ◀──ssh── collector ─▶ SQLite ─▶ queue (tailnet HTTPS)
                                   │                                        └─▶ phone push
 answers ◀───── mailbox · wake · resume ◀──────── deliver ◀── you decide
```

It's one static Go binary: server, CLI, hooks and an embedded web UI, with pure-Go SQLite. Each machine appends to a local spool. The server pulls over SSH, and laptops push with `docket sync`, so no machine has to accept inbound connections.

## Get started

[docs/setup.md](docs/setup.md) walks through it. In short:

```sh
curl -fsSL https://raw.githubusercontent.com/blobspire/docket/main/scripts/install.sh | sh
docket init --agent --person Sam            # every machine with agents
claude plugin marketplace add blobspire/docket && claude plugin install docket@docket
docket init --server --person Sam --install-service   # the always-on machine
```

More:
- [Design](docs/design.md)
- [Security model](SECURITY.md)
- [Other agent CLIs](docs/adapters.md)
- [Contributing](CONTRIBUTING.md)

## License

MIT
