# Devlog 13 - 2026-09-25: YouTube Shorts repost rolled back

Local-only rollback on branch
`feat/local-youtube-shorts-owner-summarize`, following the same session as
[devlog 12](12_owner_summarize_and_shorts.md). No push and no PR.

## Trigger

Two live failures against the desktop deployment after the commit `6fcf601`
restart:

1. First Short (`youtube.com/shorts/dspcs6gINwA`, 23:22:44 MSK): detection
   succeeded, the download layer failed after three attempts. The journal
   named the stale standalone `yt-dlp 2026.03.17`.
2. Repeat of the same link after the PATH entry was switched to
   `/usr/bin/yt-dlp 2026.08.19`: the same download-layer failure, this time
   with the terminal error visible -
   `ERROR: Sign in to confirm you're not a bot. Use --cookies-...`.

This confirmed the failure is not the yt-dlp version: YouTube now requires
bot authentication (cookies or a PO token) for this serverless download
path.

## Decision

The owner evaluated the options (cookies, PO-token authentication) and
rejected them: **"никак, откатывай это"**. The Shorts download/repost
feature is rolled back entirely. Supersedes the Shorts portion of
[devlog 12](12_owner_summarize_and_shorts.md); devlog 12 is immutable and
left as written.

## Rollback contract

- Explicit `/shorts/<id>` messages go through the existing `si=` sanitizer
  like any other YouTube URL; when `si=` is present the link is cleaned and
  reposted under the old contract.
- Owner-only `/summarize` (commit `8a24e34`) is preserved untouched.
- TikTok, X-post, and generic failure behavior are unchanged.

## Documentation state at this devlog

The rollback deletes `internal/bot/youtube_shorts.go` and restores
`internal/bot/youtube_sanitizer.go` plus the YouTube middleware comment
in `internal/bot/routes.go`. At this snapshot it is written but pending
orchestrator deployment; the desktop unit still runs the Shorts-capable
commit `6fcf601`.
