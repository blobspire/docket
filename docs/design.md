# docket design

## The problem

People who run many coding agents (several projects, several machines, sometimes several accounts) lose track of **what the agents are blocked on that only they can do**. Agents mention these things in chat, where they get buried:
- "I'm holding this until you approve"
- "you'll need to log in again"
- "the drive needs replacing"

They're also lost across context compactions and scattered over sessions. The person finds out days later.

Some observations that shaped the design:
- **Agents rarely ask outright.** Most "needs you" moments are statements, not questions ending in "?", so watching for questions misses them.
- **The wait has a long tail.** Usually you answer within minutes, but sometimes an agent waits most of a day.
- **Silent failures cost more than slow answers.** An expired login, a dead session or a timer that quietly expired can stall work for days.
- **Tokens are the scarce resource.** Having a model read every transcript to find asks is too expensive to run all the time.

## Goals

- **One prioritized queue** of decisions, approvals, reviews (with screenshots and video), human-only tasks and health problems, across all machines and accounts.
- **Phone push only** when a lead agent is fully blocked, a system is down, or something is due today.
- **Your answer goes back right away,** by the fastest path that works, so the work continues.
- **Agents stay in charge of their own memory.** docket doesn't re-inject reminders after compaction. Its store is what guarantees *you* see every open item.
- **No extra model tokens for capture.**

## Architecture

```
 each machine                                       server (always on)
 Claude Code hooks ─▶ docket hook ─┐                docket serve
 agents ──────────▶ docket ask ────┼─▶ spool/ ◀─ssh─ collector ─▶ SQLite ─▶ web UI (tailnet)
                                   │                     ▲               └─▶ push (ntfy)
 mailbox/ ◀──────────────────────────────── ssh ──── deliver ◀── you answer
 tmux sessions ◀─────────────────────────── ssh ──── deliver / lead wake
```

- **Spool:** hourly append-only JSONL segments, written under `flock`. Each record has a ULID id, the machine, the event, the session id, the working directory, tmux location, and the hook payload with long strings truncated. Evidence files are stored by content hash.
- **Pull protocol:** `docket spool export --cursor C` returns only complete lines plus a trailer with the next cursor. The server stores the records (`INSERT OR IGNORE` by id) and the cursor in one transaction, then sends `docket spool ack C`. Delivery is at-least-once and de-duplicated.
- **Direction:** the server opens every connection with the system `ssh`. Laptops that sleep and lab machines that can't reach the server still work.

## Capture: agents decide

The first version tried to catch every ask: a regex over agents' messages, a transcript grep, an item for every permission prompt and auto-mode refusal, and a model that annotated each item. The result was a queue of 84 items, about 10 of them real, which the human stopped reading. Agents are capable of judging what needs the human if they're told why it matters, so capture is now:

1. **Instructions.** A short managed block in the global `~/.claude/CLAUDE.md`:
   - why the human's attention is scarce,
   - the bar (results they're waiting on, decisions the agent can't make, things only they can do, things that broke),
   - what doesn't belong,
   - to ask in chat when the human is right there and close the item when it's settled in chat.

   The `docket` skill has the details: projects, urgency, titles, and examples from a real queue.
2. **The CLI corrects at filing time.** A missing or unknown project gets an error listing the projects. A long or multi-line title gets an error that says where the details go.
3. **Feedback.** "Not worth my time" (with an optional note) closes an item, tells the filing session without waking it, and joins the list of recent examples in `~/.docket/rules.md`, which every agent reads.
4. **Hooks only for what an agent can't file:**
   - a Claude login or billing failure (one item per machine, urgency `now`);
   - a lead (unattended by definition) waiting on a dialog for more than 15 minutes;
   - otherwise session state for delivery.

## Projects, places, urgency

- **Projects** are chosen by agents.
  - `~/.docket/context.json`, which the server sends every machine, lists the projects with descriptions, components and places in use. It also lists the folder→project map and that machine's own open items.
  - The first explicit filing from a repository maps that repository (worktrees map to their main repo), so later filings can leave `--project` out.
  - The human can rename or merge projects from the docket.
- **Places** group things the human must do in person, so one trip covers them. Agents name the place; docket doesn't guess it from the text.
- **Urgency:**
  - `now` puts an item in the red band and pushes the phone 90 seconds after it becomes urgent, at any hour.
  - `soon` and `normal` never push.
  - Deadlines are only for sorting; nothing escalates on its own.
- **Expiry** closes an item when it stops mattering.

## The message, not the record

docket is how agents reach the human, not where their work is kept. Sessions run for days and the human can't follow them, so what reaches docket is all they see; but the project owns the record, kept its own way (a repository, results folders, an experiment database with commit hashes), and docket never reads or writes it.

- **The instructions say so.** Agents keep results in the project, file a summary written for the human, and say where the full version lives with `--record` (a path with a commit, or a URL). Neither the filing agent nor a later one can read docket back.
- **docket keeps what it was sent.** Items, their bodies and copies of their evidence stay in the database after Done (Search finds them); they're the human's reading copy, never the source.
- **Nightly backups.** The server writes a gzipped snapshot of its database (`VACUUM INTO`) to `<data_dir>/backups` after 4:00 each day and keeps `backup_days` of them (default 7; 0 turns them off). They guard against a bad upgrade or a damaged file, not a lost disk. Evidence files aren't copied: they're named by content and never rewritten. To restore one: stop docket, delete `docket.db-wal` and `docket.db-shm` next to the database (a crash can leave them, and they'd be applied to the restored file), then `gunzip -c` the backup over `docket.db` and start docket.

## Progress, the timeline, and asking later

- **Progress** (`docket progress "<what's done>" --next "<what's starting>"`): when an agent finishes a major stage it posts a sentence or two. It's stored as an item with its own status, `logged`, so everything that gathers what needs the human (the docket, the badge, pushes, expiry, the agents' context, the Done page) skips it by construction; search still finds it. The docket shows the last three days in one folded Progress list under the action items. The human can ask about an entry or mark it not worth their time, nothing else.
- **The timeline** (`/timeline?p=<project>`): a project's history, by day, newest first, built from what docket already keeps: results (reviews not dismissed, done or not), progress, problems, the human's decisions, and ideas that shipped (with their pull request). **Results** is one of its filters, so a result from months ago is two taps away.
- **Ask the agent**: on any item the human has dealt with (closed, or a progress entry) that came from an agent's session, a question box sends the question to that session by the usual paths (waking or resuming it; a laptop's ended session is resumed there at its next sync, and after three failed tries the question waits in its mailbox). The item stays where it is: the question is a delivery of its own kind, not an answer, so it changes nothing about the item's status. The agent answers onto the item with `docket add`, and the open sheet reloads when the answer lands.

## Handing an item back

Agents sometimes file as a task something they could do themselves with the human's go-ahead: a config change outside their repository, a step auto mode refused, commands written up for the human to run. Two things keep that from costing the human a terminal:

- **The skill** tells agents that anything they could do once the human says yes is an `ask` with a one-tap option ("Do it"), not a `task`. Tasks are for what only a person can do.
- **"Have the agent do it"** on any task or issue an agent filed, in the dock and on its row (`h`). Not on one filed with a place (or `--physical`): the agent already said it needs the human there. Words typed in the reply box go with it (as with an option or an approval), ahead of the go-ahead they qualify. It is an answer like any other: it goes to the filing session by the paths below (resuming it if it ended), logged as an approval, and it can be undone until it is delivered. The message is the go-ahead, including steps outside the agent's repository; the agent closes the item when it's done and files only a step that truly needs the human.

## Delivery

| Session state | Path |
|---|---|
| Busy | Mailbox, injected at the next tool call (`PostToolUse` additionalContext) or at turn end (Stop continuation) |
| Idle, any live session | An async Stop-hook waiter (`asyncRewake`) sleeps until an answer lands, then wakes the session |
| Idle, started before the plugin, in tmux | Bracketed paste, guarded as above |
| Dead | Auto-resume with the answer |
| Machine offline | Delivered as soon as it is reachable |

## Leads and the chief of staff

- **Leads:** one long-running agent per area, defined in the server's private config with a `LEAD.md` charter in each repo.
  - Leads are event-driven by default: woken by answers, job events, or optional systemd timers that survive reboots.
  - Session control matches the tmux conventions of rc, a small standalone tool for remote Claude Code sessions.
- **Chief of staff:** deterministic.
  - It runs a watchdog: unreachable machines, a lead stuck on a dialog, expired logins, stale jobs, quota.
  - It orders the queue and expires moot items.
  - It pushes act-now items.
- **Brain:** one optional, capped model job proposes preference rules from the human's corrections, decisions and "not worth my time" notes. The human approves each rule. It never answers an item.
- **Ideas** (see [ideas.md](ideas.md)): the human dictates or types an idea; docket transcribes it on a self-hosted whisper.cpp server, and a router (the second model job: one capped `claude -p` call per dictation, started by the human, never part of capture) splits it into ideas and picks a route for each. Ideas that need work get a session of their own in the project's repo, which proposes a plan as an ordinary item; **Build it** is the approval to build, merge and release. The router only routes, and docket checks its answer; nothing is built without the human's tap or their explicit "just build it".
- **Email** (see [email.md](email.md)): three passes a day read each mailbox (read-only), close reply items you answered from your own mail app, and sort new threads with one tool-less model call per batch into reply, digest or skip. Replies are items in a reserved `email` project shown at the bottom of the docket (never urgent, never counted in the badge); the digest is a separate list that fades. Each reply is drafted by a read-only, sandboxed session; docket cleans the HTML, picks the recipients and sends only on the human's tap. Work an email asks for becomes an idea proposal, and the build session gets the approved plan only. See [SECURITY.md](../SECURITY.md#email).
- **No keyword guards.** docket relays the human's answer as given. Whether a robot may move is for the agent that asks and the robot's own safety layer, which bind every path to it; a word list on the docket can't tell an experiment arm from a robot arm.

## The docket's actions

| Action | What happens | The agent hears |
|---|---|---|
| An option, Approve, Decline, a reply | The answer is delivered; the item waits under "Waiting on agents" until the agent closes it (or a day after delivery) | The answer, right away |
| Done | Closed | That it's done, quietly at its next turn (a task, or anything it's paused on: right away). An unanswered question is not a yes. |
| Not worth it | Closed, with an optional note | The note, quietly; every agent sees it in `rules.md` |
| Dismiss | Closed | Nothing |
| Stash | Kept on the Stash for 30 days, then in Done; an open item is also marked done | As for Done |
| Snooze | Hidden until tomorrow at 9:00 (or an hour) | Nothing |
| Ask the agent (an item already dealt with) | Nothing changes; the question shows on the item until the answer is added | The question, right away (waking or resuming it) |

## What was learned building it

`docs/spikes.md` records the experiments behind the design. The short version:
- an async Stop hook can wake an idle session;
- Claude Code's session registry gives state, tmux pane and Remote Control link for every live session;
- a `permission_prompt` notification doesn't name the command, but `PermissionRequest` does.

## Milestones

All of these have shipped in their first form:

- **M0, capture:** spool, hooks, plugin, CLI filing, spikes.
- **M1, the queue:** server, collector, deterministic items, CLAUDE.md block and skill, self-check, backfill, web queue, push.
- **M2, immediate delivery on every path:** evidence library, human checklist, decision search.
- **M3, leads:** wake timers, job liveness.
- **M4, chief of staff:** watchdog, quota, morning brief, pre-review.
- **M5, brain:** decision log, proposed rules for the human to approve.
- **M6, release:** packaging, docs, adapter guide.
- **M7, the dashboard:**
  - agents decide what to file: no scraping, no pre-review, no morning brief;
  - projects, components, places, urgency and expiry;
  - act-now-only pushes;
  - "not worth my time" feedback;
  - the per-project dashboard.
