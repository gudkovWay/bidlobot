# Handoff - 2026-09-25 (owner-only summarize + YouTube Shorts deployed locally)

## 1. Current state

- BidloBot is a Go Telegram supergroup bot for stats, mini-games,
  reputation, referrals, captcha/admission, content reposts and
  owner-triggered chat summaries.
- The live deployment is the owner's desktop user service:
  `bidlobot.service`, binary `~/.local/lib/bidlobot/bin/bidlobot`, env
  `~/.config/bidlobot/env`, data `~/.local/share/bidlobot/`.
- The service is **active/running** after the 2026-09-25 23:05:56 MSK
  restart. PID observed after restart: `122310`.
- The deployed binary reports `version=local commit=6fcf601`, authenticated
  as `bidlolocal_bot`, restored the summarize window and entered Telegram
  polling. The previous binary is preserved as `bidlobot.prev` and reports
  commit `d9d3c48`.
- Source is local-only in worktree
  `/home/q/dev/qol/.worktrees/bidlobot/feat/local-youtube-shorts-owner-summarize`,
  branch `feat/local-youtube-shorts-owner-summarize`, base `d9d3c48`.
  No push and no PR.
- Feature commits:
  - `8a24e34 feat(summarize): restrict digests to bot owner`
  - `6fcf601 feat(youtube): repost Shorts as Telegram videos`

## 2. Behavior delivered

### Owner-only `/summarize` and `/итог`

- Authorization is the exact `App.botOwnerID` loaded from the required
  `BOT_OWNER_ID`; no numeric Telegram ID is hardcoded.
- Non-owners, including group admins and anonymous admins, receive no reply.
- The command bypasses `gateMsg` so unauthorized calls cannot produce its
  bounded cooldown notice. Authorized calls still use the internal
  summarize cooldown.
- Deferred summarize jobs created before this cutover are discarded during
  `/flush` when their recorded user is not the current bot owner. This
  prevents the deferred path from bypassing the new authorization rule or
  spending provider budget.
- Help, command-menu and onboarding copy describe the command as owner-only.

### YouTube Shorts video repost

- The existing `youtubeSanitizer` first detects explicit recognized YouTube
  URLs whose path is exactly `/shorts/<id>`. URL entities and visible text or
  captions are supported; scheme-less links are normalized.
- A recognized Short starts a panic-contained background job and skips the
  old `si=` sanitizer for that update, preventing double handling.
- `youtu.be` is deliberately not inferred to be a Short. Non-Short YouTube
  links retain the previous `si=` sanitizer behavior.
- Download uses the pinned PATH `yt-dlp` with Deno preferred and Bun as the
  container fallback. There are three isolated attempts, each with a 45 s
  deadline and a 2 s delay between attempts. Runtime remote components are
  not enabled.
- The accepted media is a combined MP4/H.264 stream with neither dimension
  above 1280, sorted toward the 720p equivalent, and capped at 50 MiB.
  `ffprobeHasAudio` applies the existing audio validation policy.
- The bot sends the video with an attributed, HTML-safe, UTF-16-clamped
  caption. It deletes the original only after Telegram accepts the video.
- Temp-dir, download, output selection, size, audio, open or send failure
  posts the existing decline notice, keeps the original and creates no
  deferred job.

## 3. Host prerequisites observed

- `~/.config/bidlobot/env` defines `BOT_OWNER_ID`,
  `SUMMARIZE_PERSIST=1`, `SUMMARIZE_PERSIST_TTL=168h`, the Pi/OMP binary and
  captcha configuration. The owner value is intentionally not copied into
  source or docs.
- The user unit prepends `~/.local/lib/bidlobot/bin` to PATH. It therefore
  resolves pinned `yt-dlp 2026.03.17`, not the system yt-dlp.
- Deno 2.9.7 is installed and is selected by the Shorts pipeline. Bun 1.4.2,
  Node 26.10.0, ffmpeg/ffprobe 9.0.2 and OMP 18.3.1 were also observed.
- `TMPDIR=~/.local/share/bidlobot/tmp`, keeping media scratch files off the
  host tmpfs.

## 4. Verification evidence

Observed against the integrated source before deployment:

- `go build ./...` under a user systemd scope capped at 12 GiB RAM and 2 GiB
  swap: exit 0, no output.
- `go vet ./...` under the same cap: exit 0, no output.
- `gofmt -l` over every changed Go file: exit 0, no output.
- `git diff --check`: exit 0, no output.
- `docs/llm/validate.sh`: exit 0; it reported only the pre-existing stale-spec
  queue plus a temporary uncommitted-file warning before the Shorts source
  commit.
- Final independent review: no Critical or Important findings; confidence
  0.99.
- Deploy build identity before installation:
  `bidlobot version=local commit=6fcf601`.
- Post-restart service evidence: `ActiveState=active`, `SubState=running`,
  startup log names commit `6fcf601`, authentication succeeded, summarize
  state restored and polling started.

Tests were **not run**: none were requested. No live Telegram round trip was
performed, so owner/non-owner command behavior and an actual Shorts upload
remain operational observations for the owner, not claims in this handoff.

## 5. Next checks

1. In a supergroup, owner sends `/итог`; expect the normal digest flow.
2. A non-owner, including a group admin, sends `/итог`; expect silence.
3. Post an explicit `youtube.com/shorts/<id>` URL; expect a Telegram video and
   deletion of the source message only after successful upload.
4. Post a non-Short YouTube URL carrying `si=`; expect the old sanitizer path.
5. If Shorts fails, inspect `journalctl --user -u bidlobot.service -n 50`.
   The source message must remain and no `/flush` job should appear.

## 6. Deliberate non-goals and drift

- No `youtu.be` Shorts detection, new environment variable, bbolt bucket,
  queue type or change to non-Short `si=` semantics.
- No push and no PR.
- `docs/llm/10_scope.md` is referenced by `CLAUDE.md` but was superseded and
  removed; `00_index.md` points to `PRD.md` instead.
- `.omp/skills/bidlobot-deploy/` still describes the Docker/VM100 path; the
  current production instance is the desktop user service documented in
  `70_deployment.md`.
- `ROADMAP.md` E9 still uses the historical admin-summarize wording and omits
  Shorts from its smoke list. It was outside this approved change set.

## 7. Gotchas

- `BOT_OWNER_ID` is load-bearing for both admission and summaries. Missing or
  invalid configuration is a startup error.
- Owner-only means silence for everyone else; do not add an authorization
  error message.
- Keep pinned yt-dlp ahead of `/usr/bin` in the unit PATH. Do not add
  `--remote-components`.
- Shorts remains repost-first/delete-after-success and intentionally has no
  deferred queue.
- A second process cannot open `bidlobot.db` while the live process holds its
  exclusive lock. A second Telegram poller would also cause a 409 race.
- Never log summarize message text. With persistence enabled, text belongs
  only in `DB_PATH/summarize.db`, not the main database or backups.

## 8. Read order

1. `devlog/12_owner_summarize_and_shorts.md`
2. `45_summarize.md`
3. `55_youtube_sanitizer.md`
4. `60_architecture.md`
5. `70_deployment.md`
6. `devlog/11_summarize_persistence.md`
