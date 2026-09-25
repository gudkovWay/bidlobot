---
id: deployment
kind: guide
touches:
  - Dockerfile
  - docker-compose.yml
  - deploy/backup.sh
  - deploy/env.example
  - scripts/backup.sh
  - .github/workflows/ci.yml
  - .omp/skills/bidlobot-deploy/scripts/deploy.sh
  - .omp/skills/bidlobot-deploy/scripts/status.sh
  - cmd/bidlobot/
written: 2026-05-14
updated: 2026-09-25
---

# Deployment

Two shapes exist, and they are not equivalent:

1. **Production today: a systemd --user unit on the owner's desktop**
   (`bidlobot.service`) - see
   [Desktop systemd deployment (current production)](#desktop-systemd-deployment-current-production).
2. **The docker-compose stack** (this document, from
   [Compose stack](#compose-stack) on): a single service `bot`
   (the Go binary), single bot replica (Telegram allows exactly one
   getUpdates poller per token), named volume for bbolt, internal
   healthcheck (no host ports published), non-root container with tini
   as PID 1. Revised 2026-08-16 (xpost rework: the `xshot` Puppeteer
   sidecar was retired; the bot resolves X posts via the FixTweet API
   and downloads twimg media directly).

The compose path stays in the repo (Dockerfile + `docker-compose.yml`,
CI image build) and is the way to run the bot on a container host. It
is **not** where the bot runs right now: the live deployment was moved
off the Docker/VM100 host to the desktop systemd unit.

## Desktop systemd deployment (current production)

Observed state as of the 2026-09-13 session (see
[handoff.md](handoff.md)); treat the unit files on the machine as the
authority.

- **Unit**: `bidlobot.service`, `systemd --user` - `Type=simple`,
  `Restart=always` + `RestartSec=10s`, `KillSignal=SIGTERM`,
  `TimeoutStopSec=30s` (matches `App.ShutdownTimeout` + in-flight drain
  + bbolt close), `EnvironmentFile=%h/.config/bidlobot/env`,
  `WorkingDirectory=%h/.local/share/bidlobot`. Logs:
  `journalctl --user -u bidlobot.service`.
- **Binary**: `~/.local/lib/bidlobot/bin/bidlobot`, with the previous
  build kept next to it as `bidlobot.prev` (rollback = put it back and
  restart).
- **Env**: `~/.config/bidlobot/env` (systemd `EnvironmentFile`), not a
  compose `env_file`. It carries the required `TG_BOT_TOKEN` and
  `BOT_OWNER_ID`, the provider key `DEEPSEEK_API_KEY`, and (on this
  machine) `SUMMARIZE_PERSIST=1` with `SUMMARIZE_PERSIST_TTL=168h`.
  The owner's numeric Telegram id lives **only** here - never in the
  source.
- **Data**: `~/.local/share/bidlobot/` holds `bidlobot.db`, plus
  `summarize.db` when `SUMMARIZE_PERSIST=1`. `TMPDIR` points at
  `~/.local/share/bidlobot/tmp` so TikTok scratch files stay
  off the host tmpfs.
- **Tool PATH**: the unit prepends `~/.local/lib/bidlobot/bin` and the
  OMP bin dir ahead of the system PATH. The pre-feature private-dir
  `yt-dlp 2026.03.17` executable is restored for TikTok fallback use;
  `/usr/bin/yt-dlp 2026.08.19` is no longer selected by the unit.
  `ffmpeg`/`ffprobe` come from the host and `omp` from
  `~/.cache/.bun/bin`. The extra `yt-dlp.2026.03.17` copy is the
  rollback artifact from the failed Shorts experiment and may be
  removed independently. The compose image uses its own pinned yt-dlp
  ([Image](#image)).
- **Build**: the binary is built from the checkout with
  `-ldflags "-X main.version=local -X main.commit=$(git rev-parse --short HEAD)"`
  so `/version` and the startup log name the exact commit.
- **Restart**: `systemctl --user restart bidlobot`.
- **Stale helper**: `.omp/skills/bidlobot-deploy/` still describes the
  Docker/VM100 flow and does **not** match this deployment. Known
  drift, not yet reconciled.

## Prerequisites

Shared by both shapes: the bot token, `BOT_OWNER_ID`, `/setinline`, and
`/setprivacy`. The Docker/Compose requirement applies to the compose
path only.

- Linux host with Docker 24+ and Compose v2 (`docker compose version`).
- A bot token from `@BotFather`.
- The **owner's Telegram user ID** (`BOT_OWNER_ID`) - required at
  startup; only this user may add the bot to a chat, and it is also
  the sole authorization for `/summarize` and `/итог`
  ([45_summarize.md](45_summarize.md)). The value lives in the
  environment; it is never hardcoded in the binary.
- `/setinline` Enabled with a placeholder string (read-only launcher).
- `/setprivacy` - a **hard prerequisite now**: the content features
  (YouTube `si=` sanitizer, TikTok repost, X-post repost, summarize
  recorder) all read full message text, so privacy must be **OFF**
  (`@BotFather` -> `/setprivacy` -> Disable) and the bot **removed +
  re-added** to every chat (privacy is cached at join). With privacy
  ON the bot only sees commands/@-mentions/replies, and all content
  middlewares are silent.

## Image

`Dockerfile` in the repo root. Multi-stage:

- `golang:1.26-alpine` build stage. `CGO_ENABLED=0` (every Go
  dependency is pure Go). Build cache via BuildKit. Builds `bidlobot`,
  `bidlobot-backup`, `bidlobot-probe` into `/out`.
- `debian:bookworm-slim` runtime. Installs `bash ca-certificates curl
  ffmpeg tini tzdata unzip wget` (ffmpeg/ffprobe for the TikTok
  audio check), **yt-dlp pinned release** (2026.03.17, sha256-checked
  at build; a newer release can be pinned via `YT_DLP_VERSION` arg -
  the pin holds until upstream fixes the TikTok extractor regression,
  yt-dlp issue #17403), and **Bun 1.3.14 +
  `@oh-my-pi/pi-coding-agent` 16.3.6** (the `omp` CLI on PATH,
  version-checked at build). yt-dlp is the **fallback** TikTok source;
  the primary path is the tikwm mirror
  ([56_tiktok_repost.md](56_tiktok_repost.md)). Bun serves OMP, not the
  yt-dlp fallback. No `--remote-components` is used, so the downloader
  never fetches unpinned code at runtime. Runs as `bidlobot` (UID 65532),
  `WORKDIR /var/lib/bidlobot`, tini as PID 1,
  HEALTHCHECK on the loopback `/health`.

The `bidlobot-backup` binary stays in the image but cannot snapshot a
running bot (bbolt exclusive flock) - use `deploy/backup.sh`
(stop/cp/start) instead.

## Compose stack

`docker-compose.yml`:

- **bot**: `container_name: bidlobot`, `restart: unless-stopped`,
  `env_file: ./env` (on the deploy host: `/opt/bidlobot/env`) **plus**
  `environment: - DEEPSEEK_API_KEY` (taken from the host shell env -
  the omp CLI reads it directly, the Go binary never sees it).
  `volumes: bidlobot-data:/var/lib/bidlobot`; `stop_grace_period: 30s`
  (10s handler drain + 10s in-flight + 10s bbolt close slack);
  healthcheck `wget --spider http://127.0.0.1:8080/health` every 30s,
  60s start-period; resource caps 256 MB / 0.5 CPU; JSON log rotation
  10 MB x 5.

Host ports 8080/8081 are deliberately unmapped (occupied by other
services on the deploy host); all probes go over container loopback.

**Resolver override**: the `bot` service pins
`dns: [192.168.0.1, 1.1.1.1, 8.8.8.8]`. Without it the container
resolves through Docker's embedded resolver -> `127.0.0.53`
(systemd-resolved) -> the router, and host-side `127.0.0.53` stalls in
10-minute bursts (the cause of the DNS-failure class in the TikTok and
Telegram error counts). `.lan` names still resolve because the router
answers them. Applied with `docker compose up -d bot` (recreate).

## Environment

Required:

- `TG_BOT_TOKEN` -- format `\d+:[A-Za-z0-9_-]{35,}`; validated at
  startup, exit non-zero on bad shape.
- `BOT_OWNER_ID` -- positive int64 Telegram user id. Only this user
  may add the bot to a supergroup; a non-owner add triggers an
  immediate `LeaveChat` + owner DM notice. Missing/invalid ->
  startup error.

Optional:

- `LOG_LEVEL` -- debug|info|warn|error, default `info`.
- `DB_PATH` -- bbolt directory. Container default `/var/lib/bidlobot`.
- `HEALTH_PORT` -- /health + /version port, default `8080`; `0`
  disables the listener (breaks the compose healthcheck unless
  rewritten).
- `PI_BINARY` / `PI_MODEL` -- the summarize provider CLI + model.
  Defaults `omp` and `deepseek/deepseek-v4-flash`. The `omp` binary is
  inside the image; a missing binary is a startup failure.
- `DEEPSEEK_API_KEY` -- provider credential for the omp CLI. NOT read
  by the Go binary; compose forwards it from the host environment.
  `deploy.sh` pipes it from `pass show token/deepseek` over SSH.
- `CAPTCHA_ENABLED` (default false) / `CAPTCHA_TIMEOUT` (default `1m`,
  1m..30m) -- new-member math captcha + welcome animation.
- `CLEANUP_DAILY_AT` / `CLEANUP_GRACE` / `CLEANUP_DAILY_BATCH` --
  legacy gracekick tuning, still validated, **but the scheduler is
  idle** (no `/cleanup` command exists to seed a campaign). Harmless
  to keep or drop.
- `SUMMARIZE_PERSIST` (default **off**) / `SUMMARIZE_PERSIST_TTL`
  (default `168h`, valid only when the flag is on, range 1h..720h) --
  opt-in durable mirror of the summarize RAM window. When on, raw
  member message text is written to `DB_PATH/summarize.db` (created
  on demand) - a SECOND bbolt file, deliberately NOT `bidlobot.db`,
  so the main DB and its backups stay text-free. `TTL` bounds the
  prune horizon and the restore cutoff. See
  [45_summarize.md](45_summarize.md).

Removed env (no longer read; stale values are ignored): `GLM_API_KEY`,
`GLM_BASE_URL`, `GLM_MODEL`, `RECORD_UPDATES`, `CLEANUP_DAILY_ENABLED`,
`CLEANUP_DAILY_THRESHOLD`.

## First deploy

```sh
# On the deploy host
git clone https://github.com/veschin/bidlobot.git /opt/bidlobot
cd /opt/bidlobot
cp deploy/env.example env
$EDITOR env  # set TG_BOT_TOKEN + BOT_OWNER_ID

docker compose up -d --build
docker compose logs -f bot
```

Expect, in order:

1. `starting build=...`
2. `authenticated bot=<name> id=<n> can_read_all=<true|false> supports_inline=true`
3. `health server listening addr=:8080`
4. `bot started, polling for updates`

`can_read_all=true` is required for the content features (privacy OFF
+ re-add). If it reads `false`, flip BotFather privacy and re-add the
bot.

## Upgrade (routine)

`origin/master` is the deploy ref. Standard upgrade:

```sh
cd /opt/bidlobot
git fetch origin && git checkout master && git pull --ff-only
docker compose up -d --build
docker compose logs -f bot
```

Deploy helper: `.omp/skills/bidlobot-deploy/scripts/deploy.sh`
(git push -> SSH with `DEEPSEEK_API_KEY` from `pass show
token/deepseek` -> `docker compose up -d --build` -> health wait) and
`status.sh` (env + container status). NOTE: `status.sh` still greps
`GLM_` in the host env - stale, cosmetic only.

Verifiable upgrade facts:

- **New buckets are idempotent** (`CreateBucketIfNotExists`), no DB
  migration step. Rollback stays forward-compatible (unknown buckets
  ignored by older binaries).
- The image build pins yt-dlp and omp versions; a build failure
  (sha256 mismatch, missing binary) fails CI / the deploy build before
  it reaches prod.

**Desktop systemd equivalent** (the shape actually in production):
build in the checkout with the ldflags above, copy the result over
`~/.local/lib/bidlobot/bin/bidlobot` (moving the old binary to
`bidlobot.prev` first), then `systemctl --user restart bidlobot` and
watch `journalctl --user -u bidlobot.service -n 50`. New buckets are
just as idempotent here; the SQLite-style migration step does not
exist in either shape.

## Health and version

```sh
docker exec bidlobot wget -qO- http://127.0.0.1:8080/health
docker exec bidlobot bidlobot --version
```

`/health` returns `200 {"status":"ok"}` when: bbolt accepts a no-op
view tx; the most recent update arrived within 5 min (or startup
grace); a cached `getMe` (TTL 60s) succeeded. `/version` returns build
info (commit hash via `-X main.version=... -X main.commit=...`).

## Backup

`deploy/backup.sh` -- host-side stop / cp / start. Resolves the
volume mount via `docker volume inspect bidlobot-data`, copies
`bidlobot.db`, restarts the bot. ~10s downtime for a guaranteed-
consistent snapshot.

The script **must run as root**, for three independent reasons: it
writes into the root-owned `/var/backups`; it defaults `COMPOSE_DIR` to
`/opt/bidlobot` while the checkout lives in `/home/veschin/bidlobot`;
and the resolved source path under `/var/lib/docker/volumes` is not
readable by the deploy user. Run as a normal user it stops the bot,
prints `ERROR: /var/lib/docker/volumes/bidlobot-data/_data/bidlobot.db
missing; restarting bot then aborting` (verified 2026-09-12) and starts
the bot again - no backup, no harm.

Install as root:

```cron
17 3 * * * cd /home/veschin/bidlobot && BIDLOBOT_COMPOSE_DIR=/home/veschin/bidlobot ./deploy/backup.sh >> /var/log/bidlobot-backup.log 2>&1
```

Run it by hand once before trusting the cron:

```sh
sudo sh -c 'cd /home/veschin/bidlobot && BIDLOBOT_COMPOSE_DIR=/home/veschin/bidlobot ./deploy/backup.sh'
```

Without root, the same stop/copy/start works through the docker socket,
which is how `docker cp` reads the file out of the container:

```sh
docker stop bidlobot
docker cp bidlobot:/var/lib/bidlobot/bidlobot.db \
    "/home/veschin/bidlobot-backups/bidlobot-$(date -u +%Y%m%d-%H%M%S).db"
docker start bidlobot
```

Default destination `/var/backups/bidlobot/...`, configurable via
`BIDLOBOT_BACKUP_DIR`; 7 files kept. Failed runs exit nonzero so cron
alerts.

`summarize.db` (present only when `SUMMARIZE_PERSIST=1`) is
intentionally NOT covered by the backup job: the scripts copy
`bidlobot.db` whole, and the mirror exists so member text never
reaches the backed-up file. It is rebuilt organically from live
traffic after a restore.

**Status 2026-09-12**: no cron entry had ever been installed and no
backup existed. The first consistent snapshot was taken by hand with
the `docker cp` recipe above (2097152 bytes, bbolt page magic
`ed0cdaed` at offset 16) into `/home/veschin/bidlobot-backups/`.

## Logs

Structured JSON to stdout, Docker `json-file` driver.

```sh
docker compose logs -f bot
docker compose logs --since 1h bot | jq 'select(.level=="ERROR")'
```

What never leaks into logs: `TG_BOT_TOKEN` (telego's default replacer
redacts; never enable `WithDebug`), `DEEPSEEK_API_KEY` (the omp CLI
reads it from env; the Go binary never logs it), message text (only
chat_id/user_id/command/duration_ms). The `/summarize` feature *sends*
recent message text to the external DeepSeek provider via omp (privacy
note in [45_summarize.md](45_summarize.md)). With
`SUMMARIZE_PERSIST=1` member text IS written to disk - to the
separate `summarize.db`, never to the main DB or its backups. It never
logs message text (memfd transcript, no temp file on the provider
path).

## CI

`.github/workflows/ci.yml`: `go vet`, `go test -race -cover`, `go
build` on push/PR; `docker buildx build` (no push) so Dockerfile
regressions fail CI; `gitleaks` scan. Coverage artifact 7-day.

## Rollback

bbolt schema is forward-compatible (buckets only ever added). To roll
back:

```sh
cd /opt/bidlobot
git fetch
git checkout <previous-good-sha>
docker compose up -d --build
```

If the older binary doesn't recognize a newer bucket, it is ignored.
For destructive-schema rollbacks (rare), restore from backup with the
bot stopped:

```sh
docker compose stop bot
VOL=$(docker volume inspect -f '{{.Mountpoint}}' bidlobot-data)
cp /var/backups/bidlobot/bidlobot-<ts>.db "$VOL/bidlobot.db"
docker compose start bot
```

## Change status (2026-09-25, local only)

The owner-gated `/summarize` (commit `8a24e34`) is deployed to the
desktop user service. The YouTube Shorts repost shipped in commit
`6fcf601` failed twice live (`Sign in to confirm you're not a bot` with
both yt-dlp 2026.03.17 and 2026.08.19); the owner declined cookies /
PO-token auth and ordered a full rollback
([devlog/13_youtube_shorts_rollback.md](devlog/13_youtube_shorts_rollback.md)).

Deployed state:

- The **rollback code** (Shorts download/repost removed; sanitizer
  restored to `si=` only) is commit `84d5d89` in worktree
  `/home/q/dev/qol/.worktrees/bidlobot/feat/local-youtube-shorts-owner-summarize`
  on branch `feat/local-youtube-shorts-owner-summarize`, based on
  `d9d3c48`. Nothing is pushed to `origin` and no PR exists.
- The desktop unit runs commit `84d5d89`: `active/running`, PID
  `204587`, started 2026-09-25 23:52:06 MSK with
  `ExecMainStatus=0`. The journal confirms authentication,
  summarize-window restore, and polling startup. The previous
  Shorts-capable commit `6fcf601` binary is preserved as
  `bidlobot.prev`.
- The private-dir `yt-dlp` executable is restored to the pre-feature
  `2026.03.17` version. The temporary switch to system `2026.08.19`
  is over; the separately named `yt-dlp.2026.03.17` copy remains only
  as a removable rollback artifact.
- `~/.config/bidlobot/env` defines `BOT_OWNER_ID` (value withheld),
  `SUMMARIZE_PERSIST=1` and `SUMMARIZE_PERSIST_TTL=168h`.
- Verification succeeded: `go build ./...`, `go vet ./...`, targeted
  `gofmt -l`, `git diff --check`, and `bash docs/llm/validate.sh`
  (0 errors, 0 warnings; 24 stale queue entries). Tests were not run
  because they were not requested.

## Operational footguns

- **Single token, two processes**: stop production before running
  `cmd/probe` or any local `go run ./cmd/bidlobot` against the same
  `TG_BOT_TOKEN` (409 getUpdates race).
- **Forgetting to remove-and-re-add after `/setprivacy` flip**:
  privacy is cached at join; content middlewares stay silent until the
  bot is removed and re-added.
- **Editing `env` without restart**: compose only re-reads `env_file`
  on container recreate. `DEEPSEEK_API_KEY` comes from the host shell
  env at `up` time - after changing it, `docker compose up -d` (not
  just restart) to re-read both.
- **Backup during crash loop**: `deploy/backup.sh` exits nonzero if
  the container is not running; diagnose first, do not wrap in
  `|| true`.
- **fxtwitter unreachable**: X-post reposts decline with a random
  phrase (original message kept), other features unaffected. The
  FixTweet API is a public third-party service; sustained outages
  surface as `xpost: metadata fetch failed` in the logs.
