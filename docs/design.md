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

## Capture, cheapest first

1. **Instructions.** A short managed block in the global `~/.claude/CLAUDE.md` tells agents to file anything only the human can do with the `docket` CLI and keep working. Details live in a skill that loads on demand.
2. **Hooks (zero tokens):**
   - `PermissionDenied`: an auto-mode refusal becomes an approval item that includes the command.
   - `Notification` (`permission_prompt`, `agent_needs_input`, `idle_prompt`, `auth_*`).
   - `StopFailure` for API and login errors, and `SessionEnd`.
3. **Self-check.** A regex over the agent's final message looks for ask-like phrases. If one matches and nothing was filed that turn, the Stop hook nudges the agent once. Hit rates are tracked.
4. **Backfill.** Live sessions are asked once to file what they're waiting on, plus a local, free grep for candidates.

## Delivery

| Session state | Path |
|---|---|
| Busy | Mailbox, injected at the next tool call (`PostToolUse` additionalContext) or at turn end (Stop continuation) |
| Idle, in tmux | Bracketed paste, only when no client is attached and the input line is empty, confirmed by a nonce receipt |
| Idle, not in tmux | Async Stop-hook waiter (spike), falling back to a one-tap "open session" push |
| Dead | Auto-resume with the answer |
| Machine offline | Delivered as soon as it is reachable |

## Leads and the chief of staff

- **Leads:** one long-running agent per area, defined in the server's private config with a `LEAD.md` charter in each repo.
  - Leads are event-driven by default: woken by answers, job events, or optional systemd timers that survive reboots.
  - Session control matches the tmux conventions of rc, a small standalone tool for remote Claude Code sessions.
- **Chief of staff:** mostly deterministic.
  - It handles a watchdog (unreachable machines, dead or stuck sessions, expired logins, stale jobs), quota visibility, ranking, and a templated morning brief.
  - Optional small model jobs pre-review items against approved preference rules and propose new rules. They annotate and bounce items; they never answer for the human.
- **Safety:** items that involve physical actions (for example robot motion) can't be approved remotely.

## Milestones

- **M0, capture:** spool, hooks, plugin, CLI filing, spikes.
- **M1, the queue:** server, collector, deterministic items, CLAUDE.md block and skill, self-check, backfill, web queue, push.
- **M2, immediate delivery on every path:** evidence library, human checklist, decision search.
- **M3, leads:** wake timers, job liveness.
- **M4, chief of staff:** watchdog, quota, morning brief, pre-review.
- **M5, brain:** decision log, proposed rules for the human to approve.
- **M6, release:** packaging, docs, adapter guide.
