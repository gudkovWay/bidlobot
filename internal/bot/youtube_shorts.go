package bot

// Explicit YouTube Shorts repost.
//
// A YouTube Shorts URL (youtube.com/shorts/<id>) is a vertical video that
// Telegram renders as a plain page link. When a supergroup message
// carries one the bot downloads the video with yt-dlp, verifies size and
// audio, reposts it as a Telegram video attributed to the original
// sender, and deletes the original only after the upload succeeded.
//
// Scope is deliberately narrow: only an explicit /shorts/<id> path on a
// recognized YouTube host counts. A youtu.be/<id> link is NOT inherently
// distinguishable as a Short - it could be any video - so it stays with
// the existing `si=` sanitizer instead of being downloaded.
//
// Safety contract (identical to the TikTok repost):
//   - repost FIRST, delete the original only after a successful upload;
//   - any failure (download / stat / audio / open / send) replies with
//     the existing public decline note and leaves the original intact -
//     the user can resend the link;
//   - NO deferred queue: a failed Short is never retried later (the
//     queue types are for TikTok exports and summarize retries only);
//   - the download+upload runs on its own shared.Go goroutine under
//     context.Background(), so the sequential update loop is never
//     blocked by a multi-megabyte download and a panic cannot kill the
//     process.
//
// Design notes / documented v1 gaps (same shape as youtube_sanitizer.go):
//   - edited_message: OUT OF SCOPE. The router feeds update.Message here;
//     an edit that introduces a Shorts link is not re-processed.
//   - text_link entities: the hidden URL is used for the download, the
//     visible text is reposted as-is. We do not splice hidden URLs into
//     the visible body (same UTF-16 offset problem as the si= sanitizer).
//   - a message carrying BOTH a Short and a `si=` link is handled by this
//     pipeline only: the si= sanitizer is skipped for that update so one
//     message never triggers two reposts of the same content.
//   - media groups / albums: only the caption-bearing item is processed.
//   - reply / forward context is lost on repost.

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/veschin/bidlobot/internal/shared"
)

const (
	// youtubeShortsAttemptTimeout caps one yt-dlp invocation. Each of
	// the three attempts gets its own deadline, so a slow third attempt
	// is not cut off by whatever the first two spent (same shape as the
	// TikTok downloader).
	youtubeShortsAttemptTimeout = 45 * time.Second

	// youtubeShortsMaxFilesize is what yt-dlp's --max-filesize gets: it
	// aborts the download instead of writing a file the bot could never
	// upload. The result is stat-checked against maxVideoSize anyway, so
	// a yt-dlp that ignored the flag could not slip a huge file through.
	youtubeShortsMaxFilesize = "50M"

	// youtubeShortsFormatSelector bounds the download to ONE combined
	// (pre-muxed, audio-included) stream, preferring what Telegram plays
	// inline: MP4 container + H.264 (avc1) video.
	//
	// The resolution bound is "no side longer than 1280", NOT
	// "height<=720": a Short is almost always portrait, and yt-dlp's raw
	// height field for the 720x1280 portrait rendition is 1280 - a
	// literal height<=720 filter would silently drop every Short to its
	// 360p rendition. max(height,width) <= 1280 is the same 720p bound
	// written for both orientations (a 16:9 720p frame is 1280x720).
	//
	// "b" - not "bv*+ba" - is deliberate: a combined format needs no
	// ffmpeg merge pass, so the single file yt-dlp wrote is the file we
	// stat and upload. Every accepted result is MP4/H.264; if YouTube
	// offers no compatible combined stream, the original stays and the
	// normal decline path explains the failure.
	youtubeShortsFormatSelector = "b[height<=1280][width<=1280][ext=mp4][vcodec^=avc1]"

	// youtubeShortsSortPreference breaks ties among the formats the
	// selector above admits: closest to the 720p-equivalent first.
	// yt-dlp's `res` sort field is min(height,width), so it is
	// orientation-correct; vcodec/ext follow the same MP4+H.264
	// preference as the selector. This is a preference, never a second
	// hard bound.
	youtubeShortsSortPreference = "res:720,vcodec:h264,ext:mp4"
)

// --- Detection -----------------------------------------------------------

// youtubeShortsURLID returns the Shorts video id when raw is a URL on a
// recognized YouTube host whose path is exactly /shorts/<id>. It returns
// "" for everything else - notably a plain youtu.be/<id> link, whose
// path carries no /shorts/ segment and which therefore stays on the
// existing `si=` sanitizer path.
//
// Scheme-less forms ("youtube.com/shorts/abc") are handled by
// ensureScheme, the same helper the TikTok detector uses, because
// url.Parse folds a scheme-less host into Path and leaves Host empty.
func youtubeShortsURLID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(ensureScheme(raw))
	if err != nil || u.Host == "" || !isYouTubeHost(u.Host) {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Path, "/shorts/")
	if !ok {
		return ""
	}
	id := strings.Trim(rest, "/")
	if id == "" || strings.ContainsRune(id, '/') {
		return ""
	}
	return id
}

// youtubeShortsDecision is the pure, unit-testable gate: it applies the
// same exclusion set as the stats counter and the TikTok detector (no
// sender / bot / anonymous-admin / channel-as-sender) and returns the
// first explicit Shorts URL found, whether it is a bare token in the
// visible text/caption or the hidden URL of a url / text_link entity.
// act=false means the message must be passed through untouched.
func youtubeShortsDecision(msg *telego.Message) (act bool, shortsURL string) {
	if msg == nil {
		return false, ""
	}
	if msg.From == nil || msg.From.IsBot ||
		shared.IsAnonymousAdmin(msg.From.ID) || msg.SenderChat != nil {
		return false, ""
	}

	// Entity URLs first: a text_link Short is invisible in the text.
	for _, ents := range [][]telego.MessageEntity{msg.Entities, msg.CaptionEntities} {
		for _, e := range ents {
			if (e.Type == "url" || e.Type == "text_link") && e.URL != "" {
				if youtubeShortsURLID(e.URL) != "" {
					return true, e.URL
				}
			}
		}
	}

	// Then bare URLs in the visible text and caption.
	for _, m := range []string{msg.Text, msg.Caption} {
		for _, tok := range urlScanRe.FindAllString(m, -1) {
			core := strings.TrimRight(tok, trailingPunct)
			if youtubeShortsURLID(core) != "" {
				return true, core
			}
		}
	}

	return false, ""
}

// --- Download ------------------------------------------------------------

// youtubeShortsDownload is the download entry point; a variable so tests
// can substitute a stub (same seam as ytdlpDownload).
var youtubeShortsDownload = downloadYouTubeShortsViaYtDlp

// youtubeShortsJSRuntime is selected once at process start. Deno is
// yt-dlp's recommended runtime and is present on the desktop deployment;
// Bun is the pinned-image fallback. Keeping the choice here avoids using
// the desktop's unsupported Bun 1.4.x while preserving the container path.
var youtubeShortsJSRuntime = detectYouTubeShortsJSRuntime()

func detectYouTubeShortsJSRuntime() string {
	if _, err := exec.LookPath("deno"); err == nil {
		return "deno"
	}
	return "bun"
}

// downloadYouTubeShortsViaYtDlp runs yt-dlp up to three times, each
// attempt under its own deadline: one shared deadline would leave the
// third attempt with whatever the first two did not spend, usually
// nothing.
func downloadYouTubeShortsViaYtDlp(ctx context.Context, rawURL, workDir string) (string, error) {
	dlURL := ensureScheme(rawURL)

	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// A failed/timed-out yt-dlp process can leave .part or fragment
		// files. Isolate attempts so a later success can never return a
		// stale artifact from an earlier failure.
		attemptDir := filepath.Join(workDir, fmt.Sprintf("attempt-%d", attempt))
		if err := os.Mkdir(attemptDir, 0o700); err != nil {
			return "", fmt.Errorf("creating attempt dir: %w", err)
		}
		path, err := youtubeShortsAttempt(ctx, dlURL, attemptDir)
		if err == nil {
			return path, nil
		}
		lastErr = fmt.Errorf("yt-dlp attempt %d: %w", attempt, err)
		if attempt < maxAttempts {
			time.Sleep(2 * time.Second)
		}
	}
	return "", lastErr
}

// youtubeShortsAttempt runs one yt-dlp invocation and returns the file it
// wrote into workDir.
func youtubeShortsAttempt(ctx context.Context, dlURL, workDir string) (string, error) {
	dlCtx, cancel := context.WithTimeout(ctx, youtubeShortsAttemptTimeout)
	defer cancel()

	cmd := exec.CommandContext(dlCtx, "yt-dlp",
		// YouTube's signature/nsig challenges need a JS runtime. Prefer
		// Deno when installed; the pinned container falls back to Bun.
		// Official yt-dlp binaries bundle EJS. --remote-components is
		// deliberately NOT passed: no runtime download of unpinned code.
		"--js-runtimes", youtubeShortsJSRuntime,
		// A Shorts link can carry a watch?v=...&list=... rewrite; never
		// pull a whole playlist off one link.
		"--no-playlist",
		// Nothing here is interactive and the whole output is captured
		// for the failure log, so keep that capture bounded.
		"--no-progress",
		"--max-filesize", youtubeShortsMaxFilesize,
		"-f", youtubeShortsFormatSelector,
		"-S", youtubeShortsSortPreference,
		// Deterministic output template: exactly one file lands in the
		// per-job temp dir, so "the file yt-dlp wrote" is unambiguous.
		"-o", workDir+"/video.%(ext)s",
		dlURL,
	)

	output, err := cmd.CombinedOutput()
	if err == nil {
		entries, rdErr := os.ReadDir(workDir)
		if rdErr != nil {
			return "", fmt.Errorf("reading work dir: %w", rdErr)
		}
		var result string
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "video.") ||
				strings.HasSuffix(e.Name(), ".part") {
				continue
			}
			if result != "" {
				return "", fmt.Errorf("yt-dlp wrote multiple result files in %s", workDir)
			}
			result = filepath.Join(workDir, e.Name())
		}
		if result != "" {
			return result, nil
		}
		return "", fmt.Errorf("yt-dlp succeeded but no final video file found in %s", workDir)
	}
	return "", fmt.Errorf("%w\n%s", err, previewOutput(output, 400))
}

// --- Caption -------------------------------------------------------------

// youtubeShortsCaption builds the HTML caption of a reposted Short: the
// same attribution header the `si=` repost uses, followed by the
// message's visible text/caption with the YouTube `si` share-tracking
// parameter removed and HTML-escaped. The hidden URL of a text_link
// entity is not part of the visible body and is not reproduced.
//
// Telegram counts rendered text, not HTML source. Budget and truncate
// the plain body first, then escape it; otherwise '&' and '<' expansions
// would consume the budget and discard visible context unnecessarily.
func youtubeShortsCaption(display, visible string) string {
	header := sprintfHeader(display)
	plainHeader := "\U0001F464 " + html.UnescapeString(display) + " писал(а):"
	body := strings.TrimSpace(visible)
	// One unit goes to the "\n" joining header and body.
	if budget := telegoCaptionLimit - utf16Len(plainHeader) - 1; budget < utf16Len(body) {
		body = truncateUTF16(body, budget)
	}
	if body == "" {
		return header
	}
	return header + "\n" + html.EscapeString(body)
}

// shortsVisibleBody returns the message's visible body with YouTube `si`
// share-tracking parameters removed: the caption for a media message,
// otherwise the text. Reusing SanitizeMessageText keeps this identical to
// the sanitizer's idea of a clean link; the `changed` flag is ignored
// because a text_link-only change leaves the visible body untouched.
func shortsVisibleBody(msg *telego.Message) string {
	if msg.Caption != "" {
		cleaned, _ := SanitizeMessageText(msg.Caption, msg.CaptionEntities)
		return cleaned
	}
	cleaned, _ := SanitizeMessageText(msg.Text, msg.Entities)
	return cleaned
}

// --- Pipeline ------------------------------------------------------------

// processYouTubeShort runs the full pipeline: download (when videoPath is
// ""), size check, audio check, upload, delete-original.
//
// videoPath is "" in production; a non-empty path (a test fixture)
// bypasses the downloader. Every failure path replies with the public
// decline note and leaves the original message in place: the old link
// with its tracking param is strictly better than lost content, and
// there is no deferred queue to retry a YouTube Short later.
//
// Package-level (not a method) so it is callable without an App, exactly
// like processTikTok.
func processYouTubeShort(
	ctx context.Context,
	snd youtubeMediaSender,
	log *slog.Logger,
	owners ownerRecorder,
	msg *telego.Message,
	shortsURL string,
	videoPath string,
) {
	chatID := msg.Chat.ID
	msgID := msg.GetMessageID()

	// Temp directory for this download.
	workDir, err := os.MkdirTemp("", "bidlobot-ytshorts-")
	if err != nil {
		log.Error("youtube shorts: creating temp dir", "chat_id", chatID, "error", err)
		sendDecline(ctx, snd, log, chatID, msgID, publicPureFailure(),
			"youtube shorts: decline note send failed")
		return
	}
	defer os.RemoveAll(workDir)

	// Step 1: Download.
	if videoPath == "" {
		var dlErr error
		videoPath, dlErr = youtubeShortsDownload(ctx, shortsURL, workDir)
		if dlErr != nil {
			log.Warn("youtube shorts: download failed, declining",
				"chat_id", chatID, "url", shortsURL, "error", dlErr)
			sendDecline(ctx, snd, log, chatID, msgID, publicPureFailure(),
				"youtube shorts: decline note send failed")
			return
		}
	}
	defer os.Remove(videoPath)

	// Step 2: Size check. yt-dlp's --max-filesize is advisory: a
	// format whose size yt-dlp could not predict still lands here.
	fi, err := os.Stat(videoPath)
	if err != nil {
		log.Error("youtube shorts: stat video", "chat_id", chatID, "path", videoPath, "error", err)
		sendDecline(ctx, snd, log, chatID, msgID, publicPureFailure(),
			"youtube shorts: decline note send failed")
		return
	}
	if fi.Size() > maxVideoSize {
		log.Info("youtube shorts: video too large", "chat_id", chatID, "size", fi.Size())
		sendDecline(ctx, snd, log, chatID, msgID, publicPureFailure(),
			"youtube shorts: decline note send failed")
		return
	}

	// Step 3: Audio check. Never upload a silent video.
	if !ffprobeHasAudio(videoPath) {
		log.Warn("youtube shorts: no audio stream, declining", "chat_id", chatID, "url", shortsURL)
		sendDecline(ctx, snd, log, chatID, msgID, publicPureFailure(),
			"youtube shorts: decline note send failed")
		return
	}

	// Step 4: Open for upload.
	file, err := os.Open(videoPath)
	if err != nil {
		log.Error("youtube shorts: opening video for upload", "chat_id", chatID, "error", err)
		sendDecline(ctx, snd, log, chatID, msgID, publicPureFailure(),
			"youtube shorts: decline note send failed")
		return
	}
	defer file.Close()

	// Step 5: Repost FIRST - the original is deleted only below, after
	// this succeeded.
	display := shared.UserDisplay(msg.From.Username, msg.From.FirstName)
	sent, sendErr := snd.SendVideo(ctx, &telego.SendVideoParams{
		ChatID:    telego.ChatID{ID: chatID},
		Video:     telego.InputFile{File: file},
		Caption:   youtubeShortsCaption(display, shortsVisibleBody(msg)),
		ParseMode: telego.ModeHTML,
	})
	if sendErr != nil {
		log.Warn("youtube shorts: repost failed; leaving original intact",
			"chat_id", chatID, "error", sendErr)
		sendDecline(ctx, snd, log, chatID, msgID, publicPureFailure(),
			"youtube shorts: decline note send failed")
		return
	}

	// Reactions on the bot's video must credit the human whose link it
	// was (the bot never receives its own outgoing messages).
	if owners != nil {
		owners.RecordOwner(chatID, sent.GetMessageID(), msg.From)
	}

	// Step 6: Delete the original (only after a successful repost).
	if delErr := snd.DeleteMessage(ctx, &telego.DeleteMessageParams{
		ChatID:    telego.ChatID{ID: chatID},
		MessageID: msgID,
	}); delErr != nil {
		log.Info("youtube shorts: reposted but delete failed; original kept",
			"chat_id", chatID, "message_id", msgID, "error", delErr)
	}

	log.Info("youtube shorts: reposted", "chat_id", chatID, "message_id", msgID, "url", shortsURL)
}
