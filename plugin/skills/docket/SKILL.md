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

## Write items that can be decided without opening your session

- **Title:** the decision or action itself, phrased as a question or an imperative. Not "Update".
- **`--why`:** the context needed to decide. Assume they haven't read anything you wrote.
- **`--recommend`:** your recommendation, whenever you have one.
- **Evidence:** attach screenshots, videos, logs or reports with `--evidence`. A review with evidence gets answered much faster.

## When their answer arrives

It shows up in your context as a message starting with `[docket dk-xxxxxxxx #nonce]`. Act on it.
- If an answer approves an action that auto mode refused earlier, retry it.
- If they declined, don't do it.
- When an item is resolved, run `docket done dk-xxxxxxxx --why "<what happened>"`.

## Don't

- Don't use AskUserQuestion for these. It blocks the session, and the human may be away for hours.
- Don't file routine progress updates; those belong in your normal output.
- Don't re-file something already open. Use `--key`, or mention it once in your next message.
