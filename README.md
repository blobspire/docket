# docket

**One dashboard of what your agents actually need from you.**

> Status: **pre-alpha**, built in the open and used daily by its author across a laptop, a home server and a lab machine. It works, but expect rough edges and changes.

If you run several coding agents across projects and machines, the things only you can do get buried:
- an approval an agent is holding
- a permission prompt
- an expired login
- "you'll need to plug in the drive"
- an issue it mentioned once, 400 messages ago, before a context compaction

You find out days later, when you ask "do you need anything from me?".

docket gives your agents one place to bring you what really needs you: results you're waiting on, decisions they can't make, things only you can do, and things that broke. It groups them by project, puts anything urgent at the top, pushes your phone only when something can't wait, and delivers your answer back to the agent right away so the work continues.

## What it does

- **Agents decide what reaches you.** No scraping of chat text and no model vetting. A short instruction in every agent's context explains why your attention is scarce, sets the bar, and has agents write a title you can scan in a second.
  - Agents file with a non-blocking CLI (`docket ask`, `task`, `review`, `issue`), choosing a **project**, an optional **component**, a **place** for things you do in person, an **urgency** (`now`, `soon`, `normal`), a deadline and an expiry.
  - When you answer in chat instead, the agent closes the item itself.
  - Claude Code hooks catch only what an agent can't file itself: an expired Claude login, a stuck unattended lead.
- **A docket you can read quickly:**
  - a project sidebar (or chips on a phone)
  - a red **Act now** band at the top
  - one section per project, ordered by what's most pressing, with answers still waiting on agents folded away
  - views by place, so one trip to the lab covers everything there
  - sorting by priority, deadline or newest
  - each item opens in a sheet with its rendered markdown, images, reports and the session it came from
  - an **answer dock** pinned under every item: the agent's options (the recommended one filled in), a reply box, and the same actions in the same places (Done, Not worth it, Dismiss, Stash, Snooze), so you never scroll back up to answer
  - one-tap Done, "Not worth my time" and Dismiss on every row that needs you, so you can clear the list while scrolling it, with Undo
- **Done tells the agent.** It closes the item, and the agent that filed it hears so at its next turn, in words that fit what it asked (a review was looked at; an unanswered question isn't a yes).
- **"Have the agent do it."** A task or issue an agent filed that it could do itself, given your go-ahead, goes back to it in one tap (on the item or its row). The agent does it and closes the item, and files just the step that truly needs you, if one does.
- **Stash** keeps what you've dealt with but want close at hand (results to share, a quote to act on) off your docket for 30 days.
- **"Not worth my time" with a note.** It closes the item, tells the agent that filed it, and joins a short list every agent reads, so the bar calibrates itself.
- **Delivers your answer immediately:**
  - into a busy session at its next tool call,
  - by waking an idle session,
  - by resuming a session that has ended,
  - or as soon as a sleeping laptop reconnects.
- **Pushes to your phone (ntfy) only for act-now items,** at any hour. Nothing else buzzes.
- **Ideas: dictate it, and an agent researches it.** Tap the mic (or type), say what you want changed in any project, and stop. docket transcribes it on your own server (whisper.cpp, primed with your project names), routes each idea, and starts a session in the right repo that researches it and brings you a plan. **Build it** ships it: build, PR, merge, release. Unsent audio survives reloads and dead signal. See [docs/ideas.md](docs/ideas.md).
- **Leads:** one long-running agent per area, woken by your answers, job events or schedules that survive reboots.
- **Job liveness:** a job that stops sending heartbeats becomes an alert, and its lead is woken.
- **Watchdog:** unreachable machines, expired logins, usage limits per account, which lead is spending. It never answers anything for you.
- **Brain:** your corrections and "not worth my time" notes become proposed preference rules. You approve each one, and approved rules reach every agent.

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
