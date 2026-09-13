# Devlog 11 - 2026-09-13: summarize persistence (opt-in durable mirror)

## Trigger

Owner request: the desktop deployment must survive a reboot without
emptying the `/summarize` window. The Bot API still cannot read
history, so the only lever is persisting the RAM ring somewhere the
next process can restore from - while keeping the documented
privacy posture intact by default.

## Decisions

1. **Separate bbolt file** (`DB_PATH/summarize.db`, bucket
   `summarize_msgs`), not a bucket in `bidlobot.db`. Rationale: the
   backup job copies the main DB whole (`scripts/backup.sh` untouched),
   so a separate file guarantees member text never reaches
   `bidlobot.db` and therefore never reaches `backups/`.
2. **Opt-in flag** (`SUMMARIZE_PERSIST`, default OFF; `SUMMARIZE_PERSIST_TTL`
   default `168h`, range 1h..720h, validated only when on). Rationale:
   the default posture stays exactly RAM-only; an operator who turns
   the flag on accepts text on disk knowingly.
3. **7-day TTL.** Bounds both the prune horizon and the startup
   restore cutoff; matches the weekly rhythm of a desktop workload.

## What landed

- `internal/storage/summarize_repo.go` - second bbolt file, key
  `sm:<chat>:<seq>`, JSON `{msg_id,user_id,name,ts,text}`; batched
  `AppendRows` (atomic transaction per flush).
- `internal/domain/summarize/persister.go` - flush every 5s,
  force-flush at 500 queued rows, prune every 10m (same caps as the
  RAM ring), restore at startup before the long-poll loop, final
  flush on app-context cancel; `App.Stop()` flushes after
  `inFlight.Wait()`, before the DB closes.
- `cmd/bidlobot/config.go` - the two env vars.
- Wired in `internal/bot`/`app.go` and the `App` shutdown sequence.
- Docs: [45_summarize.md](../45_summarize.md), 60_architecture.md,
  70_deployment.md.

## Non-goals (deliberate)

- No rekey of summarize rows on group->supergroup migration
  (`MigrateChatID` skips them, same as reputation/captcha/
  deferred_jobs/admission_attempts/gracekick/game leaderboards).
- No persistence of the response cache (stays RAM-only).
- No `edited_message` tracking.

## Verification

- `go test ./...` green across all packages.
- Restart round trip covered by a test: persist rows, reopen, restore,
  assert the window (and that restore happens before any live message
  can be seeded).
- Live deploy on the desktop deployment: flag on, process restarted,
  startup log shows `summarize persistence enabled` and
  `summarize window restored chats=0 rows=0` (fresh file - the restore
  path ran, there was nothing to restore yet).
- NOT YET OBSERVED LIVE: the round trip with real chat traffic
  (message -> rows on disk -> restart -> `rows=M`, M>0). The chat was
  idle at deploy time. To confirm: let messages accumulate, then
  `systemctl --user restart bidlobot` and grep the log for
  `summarize window restored`.

## Notes

- Known loss window: a hard power loss can lose up to one flush
  interval (5s). A SIGTERM restart (systemd/deploy) loses nothing.
- A corrupt/unreadable `summarize.db` is never fatal: Warn + start
  with an empty window.
