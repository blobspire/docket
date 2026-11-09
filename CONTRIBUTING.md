# Contributing

Thanks for helping. docket is small on purpose: one Go binary, no runtime dependencies, no build step for the web UI.

## Develop

```sh
make test     # go test -race ./...
make vet      # go vet + gofmt check
sh tests/shim_test.sh   # the plugin's shell shims
make dist     # static binaries for linux/darwin × amd64/arm64
```

Packages, in data-flow order:

| Package | What it does |
|---|---|
| `internal/hook` | Claude Code hook handler: records events, delivers answers, the self-check |
| `internal/cli` | `docket ask/task/review/…` |
| `internal/spool` | the per-machine append-only log and its export/ack protocol |
| `internal/server` | collector, delivery, chief of staff, leads, brain, web UI |
| `internal/ingest` | the deterministic rules that turn records into items |
| `internal/store` | SQLite (pure Go; FTS5 for search) |

## Guidelines

- **Hooks must never break a session.** They exit 0 (except the waiter's deliberate exit 2), they are fast, and they swallow errors.
- **No model calls in capture or ingest.** Model jobs live only in `internal/server/brain.go`, and they are optional and capped.
- **docket never decides for the human.** Code that answers an item must come from a human action in the UI.
- **Tests come with behaviour changes.** The server tests run the whole loop in-process: file, collect, answer, deliver, receipt.
- **Nothing personal in the repo.** Host names, topics and leads live in `~/.config/docket/`.
