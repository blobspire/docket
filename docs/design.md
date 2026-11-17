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
