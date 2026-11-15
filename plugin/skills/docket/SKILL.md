---
name: docket
description: Reach the human through docket, their dashboard of decisions and action items, when something genuinely needs them and would otherwise be missed - results they're waiting on, a decision you can't make, something only they can do (logins, credentials, money, hardware, people, anything physical), or something broken. Read before running docket ask/task/review/issue, and when a message tagged [docket dk-…] arrives with their answer.
---

# docket: reaching the human

The human runs many agents and decides from one dashboard: docket. It groups items by project, puts anything urgent in a red **Act now** band at the top, and pushes their phone only for act-now items. Their attention is the scarcest resource in this whole system. Every item you file costs them a context switch. If docket fills with things that didn't need them, they stop reading it, and the one item that mattered gets missed. So the bar is high, and you are trusted to judge it.

## Should this go to docket?

File it when it **needs them** and **would otherwise be missed**:

| File it | Don't file it |
|---|---|
| Results they asked for or are waiting on: "LIBERO-plus results: Model A 65.5% vs control 78.6%" | Progress updates, "started X", "tests pass" |
| A decision you can't reasonably make yourself, that changes direction or costs money | A question with a sensible default: take the default, say so in your reply |
| Only they can do it: a login, credentials, money, hardware, contacting a person, anything physical | "PR merged: review report" when no decision is needed |
| Something is broken and needs them: "ARC login expired: all training jobs are failing" | Bugs you can fix, or track in the repo's issues |
| An action you're holding for their approval that you can't take yourself | Optional nice-to-haves ("optional speed-up: …") |

**If they're in the conversation with you right now**, ask in chat; don't file a copy of a quick back-and-forth. File anyway when it would matter if they stepped away: it blocks work beyond this conversation, it's urgent, or it's results they'll want to come back to. When it gets settled in chat, close it yourself with `docket done <id>`.

`~/.docket/rules.md` lists items they marked **not worth their time**, often with a note saying why. Read those notes as direct instructions about what not to file.

## Filing

```sh
docket ask "Raise the GitHub Actions spending limit so CI runs again?" --project windward \
  --options "raise to 10|slim CI only|wait for Dec 1" --recommend "raise to 10" \
  --why "$(cat <<'EOF'
CI has been off since Nov 10: the free minutes ran out.

- **raise to 10**: CI back today; $10 is a hard cap
- **slim CI only**: free, desktop tests on PRs only
- **wait**: no CI until Dec 1
EOF
)"
docket task "Plug the COLE thumb drive into aurora for a backup" --project cwant --place home --component backups
docket review "LIBERO-plus results: Model A 65.5% vs control 78.6%" --project vlm-teacher --component "Model A" --evidence report.md
docket issue "ARC login expired: all training jobs are failing" --project vlm-teacher --urgency now --key arc-auth --why "..."
```

| Command | When |
|---|---|
| `ask` | A decision, or an action you're holding for approval. Give `--options` and `--recommend` when you have them. |
| `task` | Something only they can do. `--blocks "<what it blocks>"` if it blocks work. |
| `review` | Results or work for them to look at. Attach `--evidence` (screenshots, report, video). |
| `issue` | Something broken that needs them. |

### Title: they read it in a second

At most ~80 characters (90 is a hard limit), one line, plain words. Say the decision, the action or the result itself. No paths, hashes, ids, branch names or code. Details go in `--why`.

| Bad | Good |
|---|---|
| Measure the modified tool on BOTH FR5 arms (F/T sensor + TSF-85 fingers): flange face to gripper mounting face; flange to fingertip, … | Measure the new FR5 tool on both arms |
| 2026-11-14 PR #165 merged: the suite on the new-tool arm file (31 reds to 5), five real defects fixed | (don't file: no decision needed) |
| Auto mode refused: python3 -I -c " import json m=json.load(open('/home/… | Approve reading the camera calibration file? |

### Body (`--why`): markdown, short

Lead with what you need from them and why, in a sentence or two. Then at most ~5 bullets of the context needed to decide. Use **bold** labels, real lists, and a table if it helps. Aim for under ~120 words; link or attach the long version. Assume they've read nothing you wrote elsewhere.

### Project, component, place

- `--project <name>`: run `docket projects` to see the projects, with descriptions. Reuse one when it fits. In a repo that already filed to a project, you can leave it out. If nothing fits, start one: `--project <short-name> --new-project "<one-line description>"`. A project is a body of work, not a repo: several repos can share one.
- `--component <name>`: optional, the part of the project ("FR5 hardware", "billing", "CI"). Reuse names `docket projects` shows.
- `--place <name>`: only for things they must physically be somewhere to do ("tealab", "home", "office"). Items are grouped by place so one trip covers them, because a missed physical task can cost a trip back. Reuse existing place names.

### Urgency: `--urgency now|soon|normal` (default normal)

- **now**: something is down or blocked, and every hour it waits costs real time or money until they act. Examples: a login expired and all training jobs are failing; production is down; a trading system is losing money. It goes in the red band and **pushes their phone at any hour**. Use it rarely and honestly.
- **soon**: a real deadline or a person is waiting: a reply someone needs this week, a server shutting down Friday.
- **normal**: everything else: results, reviews, decisions that don't block anything today. Most items are normal.

### Deadlines and expiry

- `--due YYYY-MM-DD` (or `today`/`tomorrow`): a real deadline. They can sort by it.
- `--expires <date | 6h | 3d>`: when the item stops mattering, so it closes on its own instead of rotting. Example: "Launch before Curry shuts down his server?" expires when the server is gone.

### Other flags

- `--key <stable-id>`: re-filing with the same key updates the one item instead of adding another: a new title, body or urgency. Escalate this way (`--urgency now`) if a waiting item becomes urgent. Use it for anything you re-check on a schedule.
- `--blocking`: you're paused until they answer.
- `--physical`: needs physical action (robots, hardware). It can never be approved remotely; they'll do it in person.

## After filing: keep the dashboard true

- `docket list` shows open items from this machine. `*` marks yours, so you can find their ids after a context compaction.
- `docket done <id> --why "<what happened>"` as soon as an item is answered in chat, done, fixed, or moot. Use `docket done --key <key>` if you filed it with a key. Leaving stale items open is the fastest way to make the dashboard useless.
- `docket add <id> --why "..." --evidence file.png` when you learn more, instead of filing again.

## When their answer arrives

It shows up as a message starting with `[docket dk-xxxxxxxx #nonce]`. Act on it:
- If they approved an action auto mode refused earlier, retry it.
- If they declined, don't do it.
- If they marked your item **not worth their time**, take their note to heart: don't re-file it, and file fewer things like it.
- When the item is resolved, run `docket done dk-xxxxxxxx --why "<what happened>"`.

## Long-running jobs

If you start something that runs for a long time (training, a backfill, a cluster job), register it so a stall gets noticed even while you're idle:

```sh
docket job register nightly-backtest --every 10m --why "3h run on the cluster"
docket job beat nightly-backtest --why "epoch 4/12"
docket job done nightly-backtest [--failed] --why "sharpe 1.4"
```

A job that misses two beats shows up on their dashboard, and its lead is woken to look. When it finishes, file the **results** with `docket review` if they're waiting on them.
