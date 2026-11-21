# Ideas: dictate it, an agent researches it, Build it ships it

You have an idea for one of your projects. Tap the mic in docket (or press `n`), say what you want in your own words, and stop. docket transcribes it on your own server, and an agent works out which project it belongs to and what should happen next. For a concrete change, a fresh session researches it in that project's code and brings you a plan. **Build it** approves the plan: the same session builds it, opens the pull request, merges and releases it. You can type instead of talking when you can't speak aloud.

```
mic or text ─▶ docket keeps the audio, transcribes it (whisper.cpp on your server)
            ─▶ router: one model call splits it into ideas and picks a route for each
                 save      the backlog (Ideas page, `docket ideas`)
                 send      a correction for a session that's already running
                 research  a new session in the project's repo researches it and proposes a plan
                 build     the same, but it builds straight away (you said "just build it")
            ─▶ the proposal is an item on your docket: Build it · Save for later · Reply · Open session
```

## How it works

- **Capture.** The page records 16 kHz mono audio and uploads it in 3-second chunks while you talk. Every chunk waits in the browser's storage until the server has it, so a reload, a locked screen or a dead signal never loses what you said: whatever is left goes up on the next page load, and a recording the page stopped sending is finished on the server after 10 minutes.
- **Send when I stop** (on by default, remembered per device) sends the transcript straight to the router. Turn it off to check the transcript before sending; a draft you walk away from waits on the Ideas page.
- **Transcription** uses a self-hosted [whisper.cpp](https://github.com/ggml-org/whisper.cpp) server. Each request carries a glossary: your name, your projects, the components and places in use, your leads and `[transcribe] vocabulary`. That's what gets names like DAgger or vlm-teacher right, which generic dictation mangles. The audio is kept for `keep_audio` (90 days by default) so it can be replayed; transcripts stay.
- **Routing.** One headless `claude -p` call per dictation, with no tools, reads the transcript, your projects (with where their code lives), the sessions running in the last two days and the ideas already in docket, then returns one entry per separate idea. docket checks what comes back: an unknown project falls back to the page you recorded on, and a "send" to a session that isn't running becomes research.
- **Research sessions.** Each idea that needs work gets its own session (tmux, Remote Control on, auto mode, `[ideas] model`), at most `max_running` at once. Its first message holds the idea, the full dictation, and what to do: research and critique it, then file `docket propose` and wait. Its items and answers work like any other session's.
- **Where it runs**, first match wins:
  1. the project's lead, on a machine docket can always reach;
  2. another folder of the project on such a machine;
  3. its lead or folder on a laptop that synced in the last 90 seconds (the start rides on the laptop's next `docket sync`);
  4. a clone of its repository in `[ideas] clone_dir` on the server, kept up to date with `git pull`, when the laptop is asleep (laptops report their repositories' remotes when they sync);
  5. with no remote to clone, it waits for the laptop;
  6. a project with no folder anywhere gets a scratch folder on the server.
- **Build it** means build, test, open the pull request, merge and release, following the repository's own rules (its CLAUDE.md wins over docket's instructions). The session stops only for what docket is for: credentials, money, irreversible data loss, hardware. It reports `docket idea done` when it has shipped, and the proposal closes.
- **Save for later** keeps the idea, and the agent's findings, on the backlog. Any agent can pick from it: `docket ideas` lists the project's saved ideas, `docket idea take <id>` claims one.
- **Cleanup.** An idea's session is stopped two hours after the idea ends. On a machine docket reaches, a session idle on its proposal for a day is stopped too; your answer resumes it.

The Ideas page shows what needs you (drafts, failures), proposals waiting on you, what's in progress, the backlog and what shipped lately. Its badge counts only drafts and failures. Proposals themselves are ordinary items on your docket (marked **Idea**), so they never push your phone.

## Set it up

### 1. A speech-to-text server

On the docket server, build whisper.cpp with your GPU's backend (Vulkan works on most NVIDIA and AMD cards; CUDA 12.x for older NVIDIA cards, since CUDA 13 dropped Pascal), and download `ggml-large-v3-turbo.bin` and the Silero VAD model `ggml-silero-v5.1.2.bin`:

```sh
git clone https://github.com/ggml-org/whisper.cpp ~/src/whisper.cpp
cd ~/src/whisper.cpp && cmake -B build -DGGML_VULKAN=1 -DCMAKE_BUILD_TYPE=Release && cmake --build build -j
sh ./models/download-ggml-model.sh large-v3-turbo ~/.local/share/whisper
curl -L -o ~/.local/share/whisper/ggml-silero-v5.1.2.bin https://huggingface.co/ggml-org/whisper-vad/resolve/main/ggml-silero-v5.1.2.bin
```

Run it as a user service with [deploy/systemd/whisper.service](../deploy/systemd/whisper.service). Keep VAD on and leave `--max-context` at its default: `-mc 0` (once a fix for whisper repeating itself over long pauses) makes it ignore the glossary, and VAD handles pauses instead.

### 2. The server config

```toml
[transcribe]
url = "http://127.0.0.1:8178/inference"
vocabulary = ["DAgger", "FR5"]   # extra words to spell right
said_as = { cwant = "quant" }    # names speech-to-text can't spell: how they sound
# keep_audio = "90d"

[ideas]
# model = "claude-opus-5-5"      # research sessions and the router
# max_running = 4                # sessions researching or building at once
# clone_dir = "~/code/git"       # where repositories are cloned on the server
# claude_args = ["--permission-mode", "auto"]
```

Without `[transcribe] url`, the capture sheet still takes typed ideas.

### 3. Upgrade every machine

Laptops need this version to start sessions handed to them at sync and to report their repositories' remotes; `claude plugin update docket@docket` brings the skill section research sessions follow.

## Commands agents use

```sh
docket propose "<what you'd build>" --idea <idea-id> --project <p> --why "<markdown>" [--recommend "Save for later"]
docket idea done <idea-id> --why "<what shipped, PR link>"
docket idea save <idea-id> --why "<what you found>"
docket idea take <idea-id>
docket ideas [--project <p>] [--all]
```
