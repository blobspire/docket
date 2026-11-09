---
name: docket
description: File what you need from the human in docket instead of only saying it in chat. Use whenever you are holding an action for their approval, need their decision, are blocked on something only they can do (logins, credentials, hardware, money, physical work), have work ready for their review, or noticed a problem you are not fixing now. Also use when a message tagged [docket dk-…] arrives with their answer.
---

# docket: reaching the human

The human reads one prioritized queue, not your chat history. Anything you only *say* in chat can sit unseen for days, and is lost across context compactions. So when you need them, **file it** with the `docket` CLI and keep working. Filing never blocks you.

## Which command

| Situation | Command |
|---|---|
| You are holding an action until they approve it | `docket ask "Deploy the new pricing to production?" --options "deploy|wait" --recommend deploy --why "Tests pass; 3 customers asked"` |
| You need their decision between options | `docket ask "Which storage backend?" --options "sqlite|postgres" --recommend sqlite` |
| Only they can do it (login, credentials, money, hardware, anything physical) | `docket task "Re-authenticate WARP in the GUI" --blocks "lab SSH, nightly sync"` |
| Work is ready for their review | `docket review "New landing page hero" --evidence shot-desktop.png,shot-phone.png,demo.mp4` |
| A problem you noticed and aren't fixing now | `docket issue "Backups have failed for 3 nights" --why "disk full on the NAS"` |
| Something they should know, no action needed | `docket fyi "Migrated the CI to the new runner"` |

Useful flags:
- `--blocking`: you can't make progress until they answer. Use it honestly; it can wake their phone.
- `--due YYYY-MM-DD|today|tomorrow`.
- `--physical`: the step involves physical action (robots, hardware). It can never be approved remotely; they'll do it in person.
- `--key <stable-id>`: re-filing with the same key updates one item instead of adding a duplicate. Use it for things you re-check on a schedule.

## Long-running jobs

If you start something that runs for a long time (training, a backfill, a cluster job, a recording), register it so a stall gets noticed even while you're idle:

```sh
docket job register nightly-backtest --every 10m --why "3h run on the cluster"
docket job beat nightly-backtest --why "epoch 4/12"     # from the job's loop, or each time you check on it
docket job done nightly-backtest [--failed] --why "sharpe 1.4"
```

A job that misses two beats becomes a health alert, and its lead can be woken to look.

## Write items that can be decided without opening your session

- **Title:** the decision or action itself, phrased as a question or an imperative. Not "Update".
- **`--why`:** the context needed to decide. Assume they haven't read anything you wrote.
- **`--recommend`:** your recommendation, whenever you have one.
- **Evidence:** attach screenshots, videos, logs or reports with `--evidence`. A review with evidence gets answered much faster.

## Adding to an item

If you learn more after filing (results came in, you made the screenshots), add them instead of filing again:

```sh
docket add dk-xxxxxxxx --why "Benchmarks: p95 dropped from 180ms to 95ms" --evidence bench.png
```

docket's chief of staff may ask you for this: a message that says it is from the chief of staff is a request for more detail, not the human's answer. Keep working, add what it asks for, and wait for the real answer.

## When their answer arrives

It shows up in your context as a message starting with `[docket dk-xxxxxxxx #nonce]`. Act on it.
- If an answer approves an action that auto mode refused earlier, retry it.
- If they declined, don't do it.
- When an item is resolved, run `docket done dk-xxxxxxxx --why "<what happened>"`.

## Don't

- Don't use AskUserQuestion for these. It blocks the session, and the human may be away for hours.
- Don't file routine progress updates; those belong in your normal output.
- Don't re-file something already open. Use `--key`, or `docket add` to update it.
