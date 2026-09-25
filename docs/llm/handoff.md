# Handoff - 2026-09-25 (Shorts rolled back; rollback pending verification/deploy)

## 1. Current state

- BidloBot is a Go Telegram supergroup bot for stats, mini-games,
  reputation, referrals, captcha/admission, content reposts and
  owner-triggered chat summaries.
- The live deployment is the owner's desktop user service:
  `bidlobot.service`, binary `~/.local/lib/bidlobot/bin/bidlobot`, env
  `~/.config/bidlobot/env`, data `~/.local/share/bidlobot/`.
- The service is **active/running** on the Shorts-capable binary
  `commit=6fcf601`; the previous binary is preserved as `bidlobot.prev`
  (commit `d9d3c48`).
- Source is local-only in worktree
  `/home/q/dev/qol/.worktrees/bidlobot/feat/local-youtube-shorts-owner-summarize`,
  branch `feat/local-youtube-shorts-owner-summarize`, base `d9d3c48`.
  No push and no PR.
- The **YouTube Shorts download/repost feature is rolled back** in the
  working tree: `youtube_shorts.go` is deleted, the sanitizer is restored
  to `si=`-only. This rollback is **pending orchestrator verification
  and deployment** - the deployed binary still carries Shorts until the
  orchestrator builds/deploys and the final handoff is updated.
- Kept: `8a24e34 feat(summarize): restrict digests to bot owner`.
  Dropped: the `6fcf601` Shorts feature commit's code content (rollback
  not yet committed).

## 2. Why the rollback

Two live failures of the Shorts path against the desktop deployment:

1. 23:22:44 MSK, `youtube.com/shorts/dspcs6gINwA` at commit `6fcf601`:
   detection succeeded, download failed after three attempts with stale
   `yt-dlp 2026.03.17`.
2. Repeat with `/usr/bin/yt-dlp 2026.08.19`: same failure with the
   terminal error visible - `ERROR: Sign in to confirm you're not a bot.
   Use --cookies-...`.

This proved YouTube requires bot authentication (cookies or PO token)
for this serverless download path. The owner rejected both options
("никак, откатывай это"). Rollback contract: explicit `/shorts/<id>`
messages go through the existing `si=` sanitizer like any other YouTube
URL (acted on when they carry `si=`); owner-only `/summarize`, TikTok,
X-post and generic failure behavior are unchanged. See
[devlog/13_youtube_shorts_rollback.md](devlog/13_youtube_shorts_rollback.md).

## 3. Behavior (post-rollback contract)

### Owner-only `/summarize` and `/итог` (unchanged, deployed)

- Authorization is the exact `App.botOwnerID` from the required
  `BOT_OWNER_ID`; no hardcoded id. Non-owners, admins included, get no
  reply. `gateMsg` is bypassed so unauthorized calls emit no cooldown
  notice; authorized calls use the internal cooldown.
- `/flush` discards pre-cutover deferred summarize jobs whose recorded
  user is not the current bot owner.

### YouTube `si=` sanitizer (restored contract)

- One middleware, one behavior: recognized YouTube links carrying `si=`
  are reposted attributed with `si=` stripped, then the original is
  deleted. Includes `/shorts/<id>` links when they carry `si=`; a
  `si=`-free link (Shorts or otherwise) triggers nothing.
- Failure keeps the original; no deferred queue for YouTube.

## 4. Host state

- `~/.config/bidlobot/env` defines `BOT_OWNER_ID` (value withheld),
  `SUMMARIZE_PERSIST=1`, `SUMMARIZE_PERSIST_TTL=168h`.
- During diagnosis the private-dir `yt-dlp` was temporarily switched from
  `2026.03.17` to system `2026.08.19`. The pre-feature private executable
  is already restored; the separately named `yt-dlp.2026.03.17` copy is
  only a removable rollback artifact.

## 5. Next checks (after the orchestrator verifies + deploys the rollback)

1. Update this handoff so the deployed commit and behavior match the
   rolled-back source.
2. In a supergroup, owner sends `/итог`; expect the normal digest flow.
   A non-owner sends `/итог`; expect silence.
3. Post a YouTube URL with `si=` (a `/shorts/<id>?si=...` is a good
   check); expect the attributed `si=`-stripped repost and deletion.
4. Post a `si=`-free YouTube link; expect nothing.
5. Confirm the source file is absent and no active Go/build reference remains.

## 6. Deliberate non-goals and drift

- No cookies/PO-token authentication for yt-dlp - explicitly declined.
- No change to TikTok, X-post, captcha, admission or deferred-queue
  behavior.
- No push and no PR.
- `ROADMAP.md` E9 still mentions the historical admin-summarize wording
  and the Shorts smoke item; outside this change set.
- `.omp/skills/bidlobot-deploy/` still describes the Docker/VM100 path;
  known drift.

## 7. Gotchas

- `BOT_OWNER_ID` is load-bearing for admission and summaries; missing or
  invalid configuration is a startup error.
- The deployed binary still reposts Shorts as video until the rollback
  is deployed - do not treat a Shorts repost as a regression signal
  before then.
- A second process cannot open `bidlobot.db` while the live process
  holds its exclusive lock; a second poller causes a 409 race.
- Never log summarize message text; with persistence it belongs only in
  `DB_PATH/summarize.db`.

## 8. Read order

1. `devlog/13_youtube_shorts_rollback.md`
2. `devlog/12_owner_summarize_and_shorts.md` (Shorts portion superseded)
3. `45_summarize.md`
4. `55_youtube_sanitizer.md`
5. `60_architecture.md`
6. `70_deployment.md`
