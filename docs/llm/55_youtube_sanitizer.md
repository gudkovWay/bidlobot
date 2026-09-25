---
id: youtube-sanitizer
kind: spec
touches:
  - internal/bot/youtube_sanitizer.go
  - internal/bot/youtube_shorts.go
  - internal/bot/routes.go
written: 2026-05-15
updated: 2026-09-25
---

# YouTube links: `si=` sanitizer + Shorts repost

See also: [PRD.md](PRD.md), [50_telegram.md](50_telegram.md),
[56_tiktok_repost.md](56_tiktok_repost.md).

One middleware (`youtubeSanitizer`), two behaviors:

1. **`si=` sanitizer** (2026-05-15) - strip the share-tracking
   `si=` parameter that leaks who-shared-from-where.
2. **Explicit Shorts repost** (2026-09-25) - a `youtube.com/shorts/<id>`
   link is downloaded with yt-dlp and reposted as a Telegram video,
   under the same safety contract as TikTok.

Neither is the dropped "YouTube Summary" (that was an LLM/GLM
dependency, still dropped - see PRD.md).

## Wiring

`internal/bot/youtube_sanitizer.go` holds the middleware and the `si=`
path; `internal/bot/youtube_shorts.go` holds Shorts detection,
download and repost. It is a passive supergroup middleware registered
on `sgGroup` AFTER the passive observers (membership/stats/summarize
recorder must see the original human message first) and BEFORE the
TikTok reposter and X-post sidecar. The Shorts check is the first step
inside the handler: if the message qualifies, the handler starts the
async job and returns without running the `si=` path (no double
action); otherwise the `si=` path runs exactly as before. No
middleware was added and the `sgGroup.Use` order is unchanged.

The same repost-then-delete shape is reused by the TikTok reposter
([56_tiktok_repost.md](56_tiktok_repost.md)); the X-post sidecar is
add-only ([57_xpost.md](57_xpost.md)).

**Privacy gate**: like all content middlewares it needs BotFather
privacy OFF + bot re-added - a bare YouTube link never reaches the bot
under privacy ON ([50_telegram.md](50_telegram.md)).

## Detection

Scan `msg.Text`+`msg.Entities` and `msg.Caption`+`msg.CaptionEntities`
(including `url` and `text_link` entities - a `text_link` URL is in
`entity.URL`, not the visible text). Hosts are scoped strictly:
Spotify and other `si=` links, look-alike hosts
(`youtube.com.evil.com`, userinfo spoofs) are NOT touched.

- **`si=` sanitizer**: a link qualifies if its host ∈ {youtube.com,
  www/m/music.youtube.com, youtu.be, youtube-nocookie.com} AND it
  carries a `si` query param.
- **Shorts** (`youtubeShortsURLID`): the host must pass the same
  `isYouTubeHost` predicate the sanitizer uses, and the path must be
  `/shorts/<id>` with a non-empty id and no further segments.
  Scheme-less forms (`youtube.com/shorts/abc`) are accepted via
  `ensureScheme`, the same helper the TikTok detector uses. A
  `youtu.be` link has no `/shorts/` segment and is **never**
  classified as a Short - it stays on the `si=` path (or is ignored
  when it has no `si=`).

The Shorts scan checks the `url` / `text_link` entity URLs first (a
`text_link` Short is invisible in the text), then bare URLs in the
visible text and caption via `urlScanRe` with trailing punctuation
trimmed. The first qualifying URL wins.

Exclusions for both: `from == nil`, bot sender, anonymous admin,
`sender_chat` (linked channel) - the same predicate family as stats
counting.

## Shorts pipeline (download + repost)

Only for explicit Shorts URLs. Order matters:

1. **Download** into a per-job temp dir (`bidlobot-ytshorts-*` under
   `TMPDIR`, removed afterwards) with one isolated subdirectory per
   attempt, so stale `.part`/fragment files can never be mistaken for
   a later success. yt-dlp gets three attempts, a
   fresh 45s deadline per attempt and 2s between attempts:

   ```
   yt-dlp --js-runtimes <deno|bun> --no-playlist --no-progress \
          --max-filesize 50M -f '<selector>' \
          -S 'res:720,vcodec:h264,ext:mp4' \
          -o <workdir>/video.%(ext)s <url>
   ```

   The process selects the JS runtime once at startup: Deno when it is
   on `PATH` (yt-dlp's recommended runtime and the desktop path), or
   Bun otherwise (the pinned container image path). The official
   yt-dlp binary bundles the EJS solver; `--remote-components` is
   deliberately **not** passed, so no unpinned code is fetched at
   runtime. `--no-progress` keeps the captured output
   (`previewOutput`) small.
   `.%(ext)s` plus the selector keep the result a single MP4/H.264
   file - the downloader takes the one file written into the per-job
   work dir (no ffmpeg mux step). The format selector is one string:

   ```
   b[height<=1280][width<=1280][ext=mp4][vcodec^=avc1]
   ```

   The bound is `max(height,width) <= 1280`, **not** `height<=720`: a
   Short is portrait, and yt-dlp reports `height: 1280` for the
   720x1280 rendition, so a literal `height<=720` filter would silently
   degrade every Short to 360p. `1280` on the longer side is the same
   720p bound for both orientations (16:9 720p is 1280x720). `-S
   'res:720,vcodec:h264,ext:mp4'` then breaks ties among the admitted
   formats - yt-dlp's `res` is `min(height,width)`, so the preference
   is orientation-correct, and it is a preference, never a second hard
   bound. `b` (not `bv*+ba`) means no ffmpeg merge pass: the file
   yt-dlp wrote is the file the bot stats and uploads. Every accepted
   format is a combined MP4/H.264 stream; if YouTube offers none, the
   pipeline declines and keeps the original rather than uploading a
   WebM/VP9/AV1 fallback. `--no-playlist` guarantees a single video
   even when the link carries a `watch?v=...&list=...` rewrite.
2. **Size check**: `--max-filesize 50M` is a hint to yt-dlp; the code
   still `stat`s the produced file against `maxVideoSize` (50 MiB,
   the Telegram Bot API upload cap) and declines anything larger.
3. **Audio check** via `ffprobeHasAudio`; a video without an audio
   stream is declined. A missing/failing `ffprobe` degrades to
   "assume audio present", as on the TikTok path.
4. **Repost** as Telegram video through `youtubeMediaSender.SendVideo`
   (uploaded from the downloaded file, not a `file_id`) with
   `ParseMode HTML`. Caption = the same attribution header as the
   `si=` repost (`👤 <b>display</b> писал(а):`, display name only - no
   `@`, no `tg://user?id=`, no `text_mention`, honouring the
   `command-output-no-third-party-ping` invariant) + the original
   visible text/caption with `si` stripped by `SanitizeMessageText`.
   The plain body is clamped first with the project's UTF-16 helpers
   (`telegoCaptionLimit` = 1024, `truncateUTF16`, cut marked with
   `...`) and HTML-escaped only afterwards, so markup expansion does
   not consume visible-caption budget. An empty body leaves the header
   alone.
5. **Reaction attribution**: `RecordOwner` binds the bot's new video to
   the human whose link it was, so reactions on the repost credit that
   member (the bot never receives its own outgoing messages).
6. **Delete** the original ONLY after the video send succeeded. Delete
   failure is logged and the original kept (visible duplicate, lesser
   evil).
7. Runs **fire-and-forget** through `shared.Go` under
   `context.Background()`, so the synchronous update loop is never
   stalled by a multi-megabyte download/upload and a panic in the job
   cannot take the process down. Logs carry chat id / URL / error
   only - never message text.

## `si=` sanitizer action (repost-then-delete)

Unchanged for every non-Short link. A message carrying **both** a Short
and a `si=` link goes through the Shorts pipeline only: the sanitizer
is skipped for that update, so one message never produces two reposts
of the same content and the `si=` link gets no separate repost/reply
step. A bare-URL `si=` in that body is still cleaned inside the video
caption (the caption is built from the same sanitized visible body); a
`text_link` anchor keeps its hidden URL, as in the gaps below.

1. Repost FIRST as the bot: header `👤 <display> писал(а):` + the body
   with **only** `si` stripped from matched links (every other param /
   link / text preserved). Media re-sent by `file_id` (no size limit).
   Send via the rate-limited wrapper.
2. Only if the repost succeeded, `DeleteMessage` the original.
3. If the repost fails -> original is left intact (a stale `si=` link is
   strictly better than destroyed content). If delete fails (no right) ->
   reposted copy stands, original kept (visible duplicate, lesser evil).

A pure `text_link`-only change (visible body unchanged) is NOT
deleted+reposted - instead a reply with the cleaned link(s) is posted
(can't faithfully reproduce hidden-anchor text).

## Failure handling

- Shorts temp-dir / download / stat / audio / open / send failure -> the
  existing decline mechanism (`sendDecline` with a randomized phrase
  from `publicPureFailure`), the **original stays**. There is **no deferred
  queue and no new queue type for YouTube**: a retry only re-runs a
  download that just failed, and the failure catalog already gives the
  user a truthful note. Only TikTok and `/summarize` use the deferred
  queue ([60_architecture.md](60_architecture.md) "Deferred queue").
- `si=` sanitizer repost failure -> original left intact.

## Documented gaps (v1)

- `edited_message`: a clean link edited later to add `si=` is not
  sanitized (handler only sees new messages).
- Media groups/albums: only the caption-bearing item is handled;
  siblings left in place.
- Reply/forward context is lost on the bot repost (no re-threading).
- Entities are dropped on repost (offsets shift after stripping `si`);
  body re-sent as HTML-escaped text under the attribution header.
- `youtu.be` short links are never treated as Shorts (not
  distinguishable as such); they are sanitized but not downloaded.
- A Shorts repost that arrived as a `text_link` entity keeps the
  visible text only: the hidden URL is used for the download but is
  not spliced into the caption (same UTF-16 offset problem as the
  `si=` path).
- No `si=`-free, non-Short YouTube link triggers anything: the
  sanitizer only acts on `si=`, and the Shorts path only on
  `/shorts/<id>`.

## Change status (2026-09-25)

The Shorts repost is a **local, unpushed** change on branch
`feat/local-youtube-shorts-owner-summarize`; it has not been deployed
to the desktop unit. Build/vet/test/docs validation is planned after
the batch integrates - see [handoff.md](handoff.md) and
[devlog/12_owner_summarize_and_shorts.md](devlog/12_owner_summarize_and_shorts.md).
