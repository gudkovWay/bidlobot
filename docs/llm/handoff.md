# Handoff - 2026-09-13 (summarize persistence session)

## 1. State (what is true right now)

- **What the bot is.** BidloBot: a Telegram supergroup bot in Go -
  stats, mini-games, cleanup lifecycle, referral + reputation, captcha
  + owner admission gate, content reposts (YouTube `si=` sanitizer,
  TikTok via tikwm mirror + yt-dlp fallback, X-post via FixTweet), and
  owner-only `/summarize` (OMP/Pi CLI + DeepSeek V4 Flash over a RAM
  window of recent messages).
- **Deployed.** The owner's **desktop**, not the Docker host:
  systemd --user unit `bidlobot.service`, binary
  `~/.local/lib/bidlobot/bin/bidlobot` (previous binary kept as
  `bidlobot.prev`), env `~/.config/bidlobot/env`, data dir
  `~/.local/share/bidlobot/`. Commit **`dc33ba8`** ("feat(summarize):
  opt-in durable window in a separate bbolt file"), built with
  `-ldflags "-X main.version=local -X main.commit=$(git rev-parse
  --short HEAD)"`. The commit is **LOCAL ONLY - not pushed to origin**.
- **What just landed: opt-in durable summarize window.**
  - Env: `SUMMARIZE_PERSIST` (default **off**),
    `SUMMARIZE_PERSIST_TTL` (default `168h`, validated 1h..720h only
    when the flag is on). The desktop env has `SUMMARIZE_PERSIST=1` +
    `SUMMARIZE_PERSIST_TTL=168h`.
  - Storage: a **second** bbolt file `DB_PATH/summarize.db`, bucket
    `summarize_msgs`, key `sm:<absChatID %020d>:<seq %020d>` (bucket
    NextSequence), value JSON `{msg_id,user_id,name,ts,text}`.
  - **Invariant:** `bidlobot.db` and therefore `backups/` stay
    text-free. The separate file is deliberate: `scripts/backup.sh`
    copies `bidlobot.db` whole, so member text never enters the backup
    set.
  - Mechanics: `summarizeRecorder` -> `Service.Record` -> ring +
    `Persister` RAM queue; one txn per 5s flush (`FlushInterval`),
    force-flush at 500 rows (`QueueLimit`), final flush on ctx cancel;
    `App.Stop()` calls `FlushPersistence()` after `inFlight.Wait()` and
    before the DB closes; failed flush re-queues in order. Prune every
    10m enforces TTL + 2000 msgs/chat + 4 MiB/chat + 256 chats LRU.
  - Restore: synchronous at startup, before the long-poll loop;
    failure logs Warn and continues with an empty window (never
    fatal).
  - Deployment restart confirmed startup log
    `summarize persistence enabled` (ttl 168h, file path) and
    `summarize window restored chats=0 rows=0` - fresh file, no rows
    yet.
- Code: new `internal/domain/summarize/persister.go` (+test) and
  `internal/storage/summarize_repo.go` (+test); edits to `buffer.go`,
  `service.go`, `storage/keys.go`, `bot/app.go`,
  `cmd/bidlobot/{config.go,main.go}`. Docs 45/60/70 + 00_index and
  `devlog/11_summarize_persistence.md` updated in the same commit.
- Full verification at commit time: `go build ./...`, `go vet ./...`,
  `gofmt -l` clean; `go test ./...` green (26 packages).

## 2. Negatives (what is NOT true / NOT done)

- **The live organic-traffic round trip is NOT observed.** The desktop
  restart ran the restore path on a fresh file (0 rows). The restart
  round trip (persist -> close -> reopen -> restore -> per-chat
  window) is covered by `TestSummarizeWindowSurvivesRestart` with a
  real bbolt file - no claim beyond that.
- **`dc33ba8` is unpushed.** `origin/master` is still `59a2d1c`; the
  push is an explicit next step (a public repo, so it is the owner's
  call).
- **`.omp/skills/bidlobot-deploy/` is stale**: it describes the
  Docker/VM100 deploy and does not match the desktop systemd
  deployment now in place.
- **`docs/llm/10_scope.md` does not exist**, although CLAUDE.md
  references it (it was superseded by PRD.md in the v3 migration but
  the CLAUDE.md line survives).
- **`validate.sh` STALE queue has pre-existing entries** for other
  specs - not introduced by this session, still the actualization
  queue.
- Deliberate non-goals (from the session): no `MigrateChatID` rekey
  for the new bucket (documented gap, same as reputation/captcha/
  deferred_jobs), no persistence of the summary response cache,
  `edited_message` still untracked, no change to the memfd transcript
  path.

## 3. Queue

- **Push `dc33ba8`** to origin after review.
- **Observe the live round trip**: let real chat traffic accumulate,
  restart the unit, confirm `summarize window restored chats=N rows=M`
  with M>0 and that `/summarize` sees pre-restart messages.
- Reconcile `.omp/skills/bidlobot-deploy/` with the desktop systemd
  deployment (or retire it).
- Fix the stale CLAUDE.md reference to `docs/llm/10_scope.md`.
- Work the `validate.sh` STALE queue (pre-existing entries).

## 4. Gotchas (respect these)

- **Never log message text from the summarize path.** The memfd
  transcript stays; no temp file on the provider path.
- **The flag is the only thing** between the default RAM-only posture
  and member text on disk. `TestLoadConfig_SummarizePersistDefaultsOff`
  exists so the privacy default cannot silently flip.
- **A second process cannot open `bidlobot.db` while the bot runs**
  (exclusive flock). On-disk inspection needs the bot stopped.
- **bbolt never shrinks after prune** - `summarize.db` keeps its
  high-water mark.
- **Hard power loss can lose up to one flush interval (5s)** of window
  writes. SIGTERM/systemd restart loses nothing (final flush in
  `App.Stop()`).
- A corrupt/unreadable `summarize.db` is Warn + empty window, never a
  startup failure.

## 5. Read order

1. `devlog/11_summarize_persistence.md` - what landed and why.
2. `docs/llm/45_summarize.md` - window, caps, privacy, persistence.
3. `docs/llm/60_architecture.md` - layering, bbolt schema, invariants.
4. `docs/llm/70_deployment.md` - env vars, backup, `summarize.db`
   exclusion note.

## 6. Smoke test (run before touching anything)

```sh
go build ./... && go vet ./... && gofmt -l .   # expect: no output from gofmt
go test ./...                                   # expect: all packages ok
cd docs/llm && ./validate.sh                    # expect: exit 0
```

After a restart of the desktop unit, grep for exactly these two lines:

```sh
journalctl --user -u bidlobot.service -n 50 | grep -E \
  'summarize persistence enabled|summarize window restored'
# expect: "summarize persistence enabled" (ttl, file path) and
#         "summarize window restored chats=<N> rows=<M> cutoff=<ts>"
```

## 7. Agent errors

- The storage worker emitted an invalid `for ... range c.First(); ...`
  clause (Go has no range form with a post statement) - it compiles
  nowhere and was caught by the orchestrator's `gofmt -l` / `go build`
  pass, not by the worker. A test file also landed unformatted
  (`gofmt -w` needed). Lesson: workers are not allowed to build while
  sibling slices are mid-flight, so the orchestrator's post-batch
  build+go+vet pass is the only thing that catches this class.
