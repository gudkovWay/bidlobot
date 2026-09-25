---
id: youtube-sanitizer
kind: spec
touches:
  - internal/bot/youtube_sanitizer.go
  - internal/bot/routes.go
written: 2026-05-15
updated: 2026-09-25
---

# YouTube links: `si=` sanitizer

See also: [PRD.md](PRD.md), [50_telegram.md](50_telegram.md),
[56_tiktok_repost.md](56_tiktok_repost.md).

One middleware (`youtubeSanitizer`), one behavior: the **`si=` sanitizer**
(2026-05-15) strips the share-tracking `si=` parameter that leaks
who-shared-from-where, reposting the message attributed and deleting the
original. This applies to every YouTube URL that carries `si=`, including
`youtube.com/shorts/<id>` links.

An earlier (2026-09-25, rolled back the same day) attempt to download
explicit Shorts with yt-dlp and repost them as Telegram video failed twice
live with `ERROR: Sign in to confirm you're not a bot` (2026.03.17 and then
2026.08.19). YouTube requires cookies/PO-token auth for this; the owner
declined both and the feature was rolled back - see
[devlog/13_youtube_shorts_rollback.md](devlog/13_youtube_shorts_rollback.md).
It must not return without an explicit ask.

This is not the dropped "YouTube Summary" either (that was an LLM/GLM
dependency, still dropped - see PRD.md).

## Wiring

`internal/bot/youtube_sanitizer.go` holds the middleware, detection and
the `si=` path. It is a passive supergroup middleware registered on
`sgGroup` AFTER the passive observers (membership/stats/summarize
recorder must see the original human message first) and BEFORE the
TikTok reposter and X-post sidecar. No middleware was added and the
`sgGroup.Use` order is unchanged.

The repost-then-delete shape is shared with the TikTok reposter
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

A link qualifies if its host ∈ {youtube.com, www/m/music.youtube.com,
youtu.be, youtube-nocookie.com} AND it carries a `si` query param.
`youtube.com/shorts/<id>` links are ordinary YouTube links here: they are
acted on only when they carry `si=`.

Exclusions: `from == nil`, bot sender, anonymous admin,
`sender_chat` (linked channel) - the same predicate family as stats
counting.

## Sanitizer action (repost-then-delete)

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

- `si=` sanitizer repost failure -> original left intact. No deferred
  queue: only TikTok and `/summarize` use the deferred queue
  ([60_architecture.md](60_architecture.md) "Deferred queue").

## Documented gaps (v1)

- `edited_message`: a clean link edited later to add `si=` is not
  sanitized (handler only sees new messages).
- Media groups/albums: only the caption-bearing item is handled;
  siblings left in place.
- Reply/forward context is lost on the bot repost (no re-threading).
- Entities are dropped on repost (offsets shift after stripping `si`);
  body re-sent as HTML-escaped text under the attribution header.
- A `si=`-free YouTube link triggers nothing: the sanitizer only acts on
  `si=`.

## Change status (2026-09-25)

The Shorts rollback is a **local, unpushed** change on branch
`feat/local-youtube-shorts-owner-summarize`; the rollback binary has not
been deployed to the desktop unit yet - the orchestrator verifies and
deploys it. Until then the deployed binary still carries the rolled-back
Shorts feature. See [handoff.md](handoff.md) and
[devlog/13_youtube_shorts_rollback.md](devlog/13_youtube_shorts_rollback.md).
