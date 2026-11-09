# M0 spikes

Open questions about Claude Code behaviour that the design depends on, answered by experiment. Each one records what was tried, what happened, and which Claude Code version was used.

| # | Question | Status |
|---|---|---|
| S1 | Which fields does each hook event carry (Notification type, PermissionDenied reason and tool input, StopFailure error, SessionEnd reason)? Do desktop-app sessions run plugin hooks? | open |
| S2 | Does `CLAUDE_ENV_FILE` from SessionStart reach the Bash tool, so `docket ask` knows its session? | open |
| S3 | Can an `asyncRewake` Stop-hook waiter wake an idle session that isn't in tmux when an answer arrives? | open |
| S4 | Does a pasted "approved" message let auto mode run an action it denied before? | open |
| S5 | Where does a session's Remote Control URL or id appear (environment, payload, screen)? | open |
