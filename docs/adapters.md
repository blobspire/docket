# Using docket with other agent CLIs

docket was built around Claude Code, but nothing on the server is Claude-specific. The server only sees **spool records** (one JSON object per line in `~/.docket/spool/`), plus a few remote commands. To support another agent CLI, get its events into the spool and its answers out of the mailbox.

## The easy part: filing works already

Any agent that can run a shell command can use the filing CLI:

```sh
docket ask "Deploy to production?" --options "deploy|wait" --recommend deploy
docket task "Rotate the API key" --blocks "nightly sync"
docket review "New dashboard" --evidence shot.png
```

Set `DOCKET_SESSION` (a stable id for the conversation) and optionally `DOCKET_LEAD` in the agent's environment, so items are attributed and answers can be routed back. Put the instruction in whatever file your agent reads at startup; `internal/claudemd` shows the wording docket uses for Claude Code.

## Capturing what agents don't file

Map your agent's lifecycle events to these record `event` names. Write them with `docket hook <Event>` (JSON payload on stdin), or append records directly:

| docket event | When | Payload fields the server uses |
|---|---|---|
| `SessionStart` | a conversation starts or resumes | `session_id`, `cwd`, `source` |
| `UserPromptSubmit` | the human sends a message | `session_id`, `prompt` |
| `PermissionRequest` | a tool call is waiting for permission | `session_id`, `tool_name`, `tool_input` |
| `Notification` | the agent needs the human | `session_id`, `notification_type` (`permission_prompt`, `idle_prompt`, `agent_needs_input`), `message` |
| `PermissionDenied` | a safety layer refused an action | `session_id`, `tool_name`, `tool_input`, `reason` |
| `Stop` | the agent finished a turn | `session_id`, `last_assistant_message` |
| `StopFailure` | a turn failed (auth, billing, rate limit) | `session_id`, `error` |
| `SessionEnd` | the conversation ended | `session_id`, `reason` |

The record envelope is defined in `internal/record`: `v`, `id` (a ULID), `ts`, `machine`, `agent` (set it to your CLI's name), `event`, `session_id`, `cwd` and `payload`.

## Delivering answers

Answers land in `~/.docket/mailbox/inbox/<session_id>/<delivery>.json` as `{"id", "item_id", "session_id", "nonce", "text"}`. Your integration should do three things:
1. **Claim** a delivery by renaming it into `mailbox/claimed/<session_id>/`. A rename is atomic, so exactly one claimer wins.
2. **Inject** its `text` into the conversation: at the next tool call, at the next prompt, or by waking an idle session if your CLI can do that.
3. **Confirm** by appending a `docket.receipt` record with `{"nonce": ...}`.

`internal/mailbox` and `internal/hook` are the reference implementation for Claude Code.

If your CLI runs in tmux and has no hooks at all, the server can still paste answers into its pane (`docket session send`). That only happens when the pane is idle, nobody is attached, and the input line is empty.

## Session state

`docket sessions --json` reports live sessions for the server. For Claude Code it reads `~/.claude/sessions/*.json`. An adapter for another CLI should report the same shape: `session_id`, `cwd`, `status` (`busy`, `idle` or `waiting`), `tmux` and `name`.
