# M0 spikes

Open questions about Claude Code behaviour that the design depends on, answered by experiment on Claude Code 2.1.319 (Linux and macOS), 2026-11-08.

## S1. What do the hook payloads carry?

**Answered.** The payloads were captured from real sessions.
- **`Notification`** carries `notification_type` (`permission_prompt`, `idle_prompt`, …) and `message` (for example "Claude needs your permission", "Claude is waiting for your input"). `title` is optional.
  - **Gap:** a `permission_prompt` notification does **not** say which tool or command is waiting.
  - **Consequence:** docket also hooks `PermissionRequest`, which carries `tool_name` and `tool_input`, and pairs the two by session.
- **`Stop`** carries `last_assistant_message`, `stop_hook_active`, `background_tasks` (in-flight shells, subagents, monitors) and `session_crons` (CronCreate/loop wakeups). The last two are useful for job liveness and for spotting heartbeat timers.
- **`PermissionDenied`** (from the docs) carries `tool_name`, `tool_input`, `tool_use_id` and `reason` (for example `[Irreversible Local Destruction]`). Returning `hookSpecificOutput.retry: true` tells the model it may retry.
- **`StopFailure`** carries `error`: `rate_limit`, `authentication_failed`, `billing_error`, `oauth_org_not_allowed`, `account_on_hold`, `overloaded`, `server_error`, …
- **`SessionStart`** carries `source`; `SessionEnd` carries `reason`; `UserPromptSubmit` carries `prompt`.

## S2. Can a filed item be tied to its session?

**Yes, two ways.** The Bash tool sees `CLAUDE_CODE_SESSION_ID`, which Claude Code exports itself. It also sees `DOCKET_SESSION`, which docket's SessionStart hook writes to `CLAUDE_ENV_FILE`. Both equal the hook payload's `session_id`. The CLI uses `DOCKET_SESSION` and falls back to `CLAUDE_CODE_SESSION_ID`.

## S3. Can an answer wake an idle session that isn't being typed into?

**Yes. This is now the primary delivery path.**
- **Setup:** a Stop hook with `"async": true, "asyncRewake": true, "timeout": 900` waits in the background. When an answer file appears, it prints the answer to stderr and exits 2.
- **Result:** the idle session (sitting at its prompt) woke within about 1 s, showed "Stop hook feedback", and acted on the answer. The waiter re-arms at the next turn end.
- **Coverage:** this works for any live session (tmux, desktop app, Remote Control) without typing into a terminal.
- **Cleanup:** async hooks are cleaned up when the session exits; no orphaned waiters were seen.
- **tmux paste** remains the fallback, for sessions started before the plugin was installed (they have no waiter) and for dead sessions resumed in tmux.

## S4. Does an approval let auto mode run an action it refused?

**Yes, observed in real use.**
- Auto mode refused an agent action that went beyond what the user had literally asked for (adding a key to a remote `authorized_keys`, reported as `[Unauthorized Persistence]`). Once the user said "approved" in the session, the identical retry was allowed.
- The classifier weighs the user's explicit intent. When the user's prompt itself asked for a risky-looking command, it was not refused at all.
- **Consequence:** an approval delivered as the user's own words ("Cole approves: …") is the right mechanism. `PermissionDenied`'s `retry` hint is complementary.

## S5. Where is a session's Remote Control link?

**In Claude Code's session registry.**
- Each live session has `~/.claude/sessions/<pid>.json` with:
  - `sessionId`, `cwd`, `name`
  - `kind` (`interactive` or background), `status` (`idle` / `busy` / `waiting`)
  - `tmux` (`session:@window.%pane`), `messagingSocketPath`
  - `bridgeSessionId`: the open-session link is `https://claude.ai/code/<bridgeSessionId>`
- Transcripts also record `bridgeSessionId`. The hook environment does not carry it.
- **Consequence:** docket reads the registry files directly for session state and links. There's no need to run `claude agents` or scrape the screen.
