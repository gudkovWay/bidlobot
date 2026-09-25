# Devlog 12 - 2026-09-25: owner-only `/summarize` + YouTube Shorts repost

Local-only batch on branch
`feat/local-youtube-shorts-owner-summarize`, base `d9d3c48`. No push and no
PR. The feature binary was deployed to the owner's desktop user service at
commit `6fcf601`.

## Trigger

Two owner requests against the desktop deployment:

1. `/summarize` was admin-gated through `shared.AdminCache`. It should be the
   bot owner's command so group admins cannot spend the summarization
   provider budget.
2. An explicit YouTube Short should be downloaded and reposted as a Telegram
   video, matching the established TikTok user experience.

## Decisions

1. **Use the existing owner configuration.** `App.botOwnerID`, loaded from
   required `BOT_OWNER_ID`, is the authorization source. The numeric Telegram
   ID is not hardcoded.
2. **Keep unauthorized calls silent.** Non-owners, admins and anonymous
   admins receive no reply. The routes call `handleSummarize` directly so
   `gateMsg` cannot emit a bounded notice; the handler applies the existing
   cooldown after owner authorization.
3. **Close the deferred-path bypass.** `/flush` discards pre-cutover
   summarize jobs whose stored user is not the current bot owner before any
   provider call.
4. **Keep one YouTube middleware.** A Shorts pre-check is the first step in
   `youtubeSanitizer`. A matching message starts the async media job and skips
   the `si=` path, preventing double action. Other links retain the previous
   sanitizer contract.
5. **Accept only explicit Short paths.** Detection requires a recognized
   YouTube host and exact `/shorts/<id>` path. `youtu.be` links are not
   inferred to be Shorts.
6. **Require Telegram-compatible media.** yt-dlp selects a combined
   MP4/H.264 stream, neither dimension above 1280, with 720p-equivalent sort
   preference and a 50 MiB ceiling. Separate attempt directories prevent a
   failed attempt's partial output from being mistaken for a later result.
7. **Reuse media safety behavior.** Three 45 s attempts, existing
   `ffprobeHasAudio`, repost-first/delete-after-success, and the existing
   decline note on any failure. YouTube failures do not enter the deferred
   queue.
8. **Avoid unpinned runtime code.** Deno is preferred; the container's pinned
   Bun is the fallback. `--remote-components` is not passed.
9. **Preserve caption context safely.** Attribution and visible source text
   are budgeted before HTML escaping and clamped with the project's UTF-16
   helpers to Telegram's 1024-unit caption limit.

## What changed

- `internal/bot/summarize.go`, `routes.go` - direct owner authorization and
  post-authorization cooldown.
- `internal/bot/deferred.go` - removal of non-owner pre-cutover summarize
  jobs during `/flush`.
- `internal/bot/help.go`, `app.go`, `setup.go`, `membership.go` and
  `internal/text/messages.go` - owner-only copy and removal of the obsolete
  anonymous-admin error.
- `internal/bot/youtube_shorts.go` - Shorts detection, runtime selection,
  isolated yt-dlp attempts, validation, caption construction and video send.
- `internal/bot/youtube_sanitizer.go` - Shorts handoff ahead of the unchanged
  non-Short sanitizer path.
- Domain, architecture, Telegram, deployment, PRD, index and handoff docs
  were updated with the delivered contract.

## Verification and deployment

- `go build ./...`: exit 0, no output.
- `go vet ./...`: exit 0, no output.
- `gofmt -l` for every changed Go file: exit 0, no output.
- `git diff --check`: exit 0, no output.
- `docs/llm/validate.sh`: exit 0; only its stale-spec queue and a temporary
  uncommitted-file warning were reported before the Shorts source commit.
- Independent final review reported no Critical or Important findings.
- Deploy binary built with `version=local commit=6fcf601`; the prior
  `d9d3c48` binary was preserved as `bidlobot.prev`.
- `systemctl --user restart bidlobot.service` completed at 23:05:56 MSK.
  The service was observed active/running; logs showed commit `6fcf601`,
  successful Telegram authentication, summarize-window restore and polling.

Tests were not run because they were not requested. No live Telegram message
was sent during the session, so an owner/non-owner `/итог` pair and a real
Shorts upload remain manual operational checks.

## Non-goals

- No change to non-Short `si=` sanitizer semantics.
- No `youtu.be` Shorts inference.
- No new env var, database bucket or queue type.
- No push and no PR.
