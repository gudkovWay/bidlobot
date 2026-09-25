---
id: summarize
kind: spec
touches:
  - internal/bot/summarize.go
  - internal/bot/help.go
  - internal/bot/setup.go
  - internal/bot/membership.go
  - internal/bot/deferred.go
  - internal/domain/summarize/
  - internal/domain/summarize/persister.go
  - internal/storage/deferred_repo.go
  - internal/storage/summarize_repo.go
written: 2026-05-15
updated: 2026-09-25
---

# Chat summarization (`/summarize`)

Owner-only, **always on**. An LLM (DeepSeek V4 Flash via the OMP/Pi
CLI) condenses the recent chat into a short Russian digest posted
publicly in the group. Migrated 2026-07 from the Zhipu GLM HTTP
provider to the local `omp` CLI (commits caa8f55, 848ad21, 874b198).
No opt-in toggle: the feature is part of the image; a missing `omp`
binary is a **startup failure** (`exec.LookPath` in main.go, `os.Exit(1)`).

## Authorization: the bot owner, not group admins (2026-09-25)

`/summarize` and its `/итог` alias are gated to the **single configured
bot owner**, not to chat administrators:

- The gate is `App.botOwnerID`, set from the **required** `BOT_OWNER_ID`
  env var (`cmd/bidlobot/config.go` -> `App.SetBotOwnerID`, the same
  value the admission gate uses - [65_admission.md](65_admission.md),
  [70_deployment.md](70_deployment.md)).
- The check is a plain identity comparison (`msg.From.ID !=
  a.botOwnerID`). The Telegram user ID is **never hardcoded** in the
  source; it exists only in the deployment environment.
- Non-owners get **no reply** - including group admins. The
  `shared.AdminCache` lookup that previously authorized this command
  was removed from the summarize path (the cache itself still serves
  other surfaces).
- Anonymous admins cannot invoke it: their updates carry the implicit
  GroupAnonymousBot id (`1087968824`), which is not the owner id. The
  previous "disable Remain Anonymous" notice is gone - the update is
  indistinguishable from any other non-owner and is dropped silently.
- The command is still registered in the group command menu
  (`setup.go`), and the help/onboarding copy marks it as the owner's
  command, not the admins'. Command-menu visibility is not
  authorization: the check above is the only gate.

## Hard constraint that shapes everything

The Telegram **Bot API cannot read history**: no `getChatHistory`,
consumed `getUpdates` are discarded within 24h, never refetchable. A
bot can only summarize what it *kept* as messages streamed by. So
"last N" means **the last N this process heard since it started** -
not retroactive history. This is inherent, not a TODO. Combined with
the privacy model, the recorder fills only when the bot sees message
content (privacy OFF).

Persistence (`SUMMARIZE_PERSIST`, below) means a restart no longer
empties that window; it still does NOT provide retroactive history.

## Storage model: RAM ring + opt-in durable mirror

Two tiers, one read path:

- **RAM ring** (`internal/domain/summarize.Buffer`) - unchanged and
  still the ONLY read path. Per-chat ring: 2000 msgs/chat, 4 MiB
  text/chat, 256 distinct chats (LRU-evicted), oldest-evicted on
  overflow. Summaries are always built from this in-memory window.
- **Disk mirror** (`internal/storage/summarize_repo.go` +
  `internal/domain/summarize/persister.go`) - an OPT-IN durable copy
  of the same rows in a separate bbolt file. It never feeds a summary
  directly; it only re-seeds the RAM ring after a restart (below).

The Telegram constraint is unchanged: the Bot API cannot read history
(see "Hard constraint" above), so the window remains forward-only
from the first message the bot heard. Persistence removes the
**restart volatility** - a redeploy or crash no longer empties the
window - but it does NOT provide retroactive history.

### The durable mirror

Exists only when `SUMMARIZE_PERSIST=1` (default OFF).

- **File**: `DB_PATH/summarize.db` - deliberately a SECOND bbolt
  file, not a bucket in `bidlobot.db`, so member text never enters the
  main database and therefore never enters `backups/` (the backup job
  copies the main file whole; `scripts/backup.sh` is untouched).
  Created on demand.
- **Shape**: bucket `summarize_msgs`, key
  `sm:<absChatID, %020d>:<seq, %020d>` (`seq` from `bkt.NextSequence()`),
  value JSON `{msg_id, user_id, name, ts, text}` - the chat id lives
  in the key, not the value.
- **Writes** (`Service.Record` -> persister): the recorder middleware
  fills the RAM ring and queues a row; rows are flushed in ONE bbolt
  transaction every 5s (`FlushInterval`, one fsync per batch instead
  of one per message), force-flushed on a burst of 500 queued rows
  (`QueueLimit`), and flushed a final time when the app context is
  cancelled. `App.Stop()` calls `FlushPersistence()` after
  `inFlight.Wait()` and before the DB is closed, so a SIGTERM
  shutdown loses nothing. A failed `AppendRows` re-queues the rows in
  order and retries on the next tick - no row is dropped or
  duplicated (the bbolt transaction is atomic).
- **Prune** (every 10m, `PruneInterval`): enforces the SAME caps as
  the RAM ring - entries older than TTL, per-chat 2000 messages,
  per-chat 4 MiB, 256 chats (LRU by newest message time). A single
  oversized message is still kept.
- **Restore**: at startup, synchronously BEFORE the long-poll loop
  starts (a live message can never be seeded after older ones). Rows
  newer than `now - TTL` are grouped per chat and seeded through
  `Buffer.Seed`, which re-applies the RAM caps. One Info line:
  `summarize window restored` with `chats`, `rows`, `cutoff`. A
  failure logs a Warn and the bot keeps running without history - a
  corrupt window is never fatal.

### Mirror configuration

- `SUMMARIZE_PERSIST` - bool, default **OFF**. With the default, the
  documented RAM-only posture is exactly as before.
- `SUMMARIZE_PERSIST_TTL` - duration, default `168h` (7 days),
  validated only when the flag is on; range 1h..720h. Bounds both the
  prune horizon and the restore cutoff.
- Public supergroup command `/summarize [N] [questions...]`, alias
  `/итог`. The alias is matched by `textCommandPredicate`, not
  `th.CommandEqual` (ASCII-only RE2 - never matches Cyrillic).
  Typed-only (setMyCommands rejects non-ASCII names). `N` defaults to
  200 (first token after command/`@bot` parsed as int; if not a
  number, everything is treated as questions), clamped to [1, 4000]
  and to the live window size. Everything after `N` (or after the
  command if no `N`) is the **questions text**, passed to the LLM as
  additional instructions after a `---` separator; sanitized via
  `sanitizeLine` (no format injection).
- Owner-only via `App.botOwnerID` (from the required `BOT_OWNER_ID`;
  see "Authorization" above). Non-owners - admins included - get **no
  reply** (anti-spam). No admin API call or cooldown bookkeeping runs
  for them.
- Cost controls: the owner's 30s `summarizeCooldown` is applied inside
  the handler after authorization and drops repeat calls silently;
  **response cache** (RAM-only, 10-minute TTL) keyed by
  `(chatID, lastMsgID, N, questionsHash)`; per-chat single-flight
  (second call replies "уже собираю"); **process-wide ceiling 40
  calls / rolling hour** across all chats (`GlobalAllow`) - the
  provider spend stays bounded even if the owner's client loops or
  the token leaks. The expensive call runs in a tracked background
  goroutine (`App.inFlight` + app context): a placeholder message is
  posted, then `EditMessageText`-swapped in place for the result;
  SIGTERM cancels it inside the shutdown budget.

## Provider: OMP/Pi CLI

`internal/domain/summarize/pi_runner.go` runs the `omp` binary
(`PI_BINARY`, default `omp`; `PI_MODEL` default
`deepseek/deepseek-v4-flash`) with `--mode json --no-session
--no-tools --no-lsp --no-extensions --no-skills --no-rules
--thinking=minimal -p --system-prompt <sp> --model <model>`. The
transcript is passed as an anonymous memfd (`unix.MemfdCreate`) via fd
3 - nothing on disk, no temp file. Output is parsed as NDJSON:
`message_end` events, cost from `message.usage.cost.total`. stderr is
discarded (may carry provider details). **Memory bound**: per-event
scanner cap 4 MiB (`maxOMPJSONEventBytes`) - a code-dense transcript
cannot OOM the process.

The provider credential (`DEEPSEEK_API_KEY`) is read by the `omp` CLI
from its own environment - the Go binary never sees, parses, or logs
it. Compose forwards it from the host env ([70_deployment.md](70_deployment.md)).

## Token budget

Input budget 120K estimated tokens (rune/2 estimate - deliberately
rune- not byte-based; a bytes/4 heuristic mis-budgets Russian badly),
max output 2048, per-call timeout 180s. NOT a guaranteed upper bound:
code/URLs can exceed 0.5 token/rune, so the provider's own context
limit is the hard backstop (mapped to "lower N").

## Weighted digest with cost (848ad21)

The system prompt (`prompt.go`) instructs relevance-weighted
selection: threads contributing <5% of the transcript are omitted
unless they carry a decision/action/high-impact fact; 2-4 paragraphs
<1800 chars; questions answered after a `---` separator. The footer
discloses provenance + price:

```
итог M сообщений (HH:MM-HH:MM МСК), сгенерировано DeepSeek V4 Flash
via Pi по запросу @X
расчетная стоимость: $Y
```

Cost is 4-decimal from provider usage metadata; cached summaries keep
their original generation cost.

## Output

Plain text only (no ParseMode): the model is untrusted and the result
is posted publicly - markup/entities from it must not be interpreted.
Telegram still auto-links a bare `@username`, so the transcript feeds
plain names (no leading `@`) and the final body+footer is run through
`defuseMentions` (U+2060 WORD JOINER after every `@`, invisible,
breaks the mention parse). Russian, sectioned, hard-truncated at 3500
runes.

## Error taxonomy (user-facing, Russian)

| Cause | Sentinel | Owner sees |
|-------|----------|------------|
| provider/LLM failure | `ErrProviderFailure` | временная ошибка |
| ctx deadline | `ErrTimeout` | не успел, меньшее N |
| empty window | `ErrNoMessages` | пока нечего суммировать |
| already running (single-flight) | `ErrBusy` | уже собираю |
| not configured (unwired test app) | `MsgSummarizeNotConfigured` | не настроено |

The old GLM sentinels (`ErrAuth`/`ErrQuota`/`ErrRateLimited`/
`ErrContextTooLong` and the 1113/Coding-Plan lore) are gone with the
provider. `ErrSummarizeAuth/Quota/RateLimited/TooLong` strings remain
in `text/messages.go` but are no longer produced.

## Deferred retry

On provider failure the job is persisted to the **per-user deferred
queue** (`deferred_jobs`, type `summarize`, payload
`SummarizePayload{N, Questions, PlaceholderID, Requester}`) - the
placeholder stays up. The owner runs `/flush` to retry
(`retrySummarize` re-runs the summary and edits the placeholder with a
25s edit timeout). Success deletes the job. Jobs created for a group
admin before the owner-only cutover are deleted without provider work
when that user runs `/flush`; deferred retry cannot bypass the current
owner gate. See [60_architecture.md](60_architecture.md) "Deferred
queue".

## Privacy

`/summarize` sends recent member message text to an external provider
(DeepSeek) over TLS via the omp CLI. Deliberate, owner-approved
tradeoff for one owner-gated feature, mitigated by: explicit
in-message provenance + cost footer, key never seen/logged by the
bot, memfd transcript (no temp file). Owners should disclose this to
their community. The recorder needs privacy OFF to see message
content.

Text on disk is flag-conditional: with `SUMMARIZE_PERSIST=1` raw
member message text IS written to disk - but only to the separate
`summarize.db` file, so the main DB (`bidlobot.db`) and its backups
stay text-free. Default OFF, so the default posture is unchanged.
The persister logs counts/ids only - the "never log message text"
rule is untouched.

## Documented limitations (v1)

1. **Forward-only.** Only messages heard since process start; not
   retroactive. Restart-volatility is conditional on
   `SUMMARIZE_PERSIST` (default OFF empties the window on restart;
   ON survives a graceful restart, see the durable mirror above).
2. **Summarize rows are NOT rekeyed on a group->supergroup
   migration** (`MigrateChatID` skips them), same as
   reputation/captcha/deferred_jobs/admission_attempts/gracekick/game
   leaderboards ([60_architecture.md](60_architecture.md)).
3. **A hard power loss can lose up to one flush interval (5s)** of
   queued rows; a SIGTERM restart (systemd/deploy) loses nothing
   (final flush before DB close). The window remains bounded by TTL
   and the caps.
4. **Edits & deletions not tracked.** No `edited_message` subscription;
   deleted messages stay in the window until evicted.
5. **Anonymous admins cannot invoke it** - and neither can anyone else
   who is not the configured owner (`BOT_OWNER_ID`). There is no
   admin fallback: the group's admins have no access to the provider
   spend.
6. **Times are MSK** in the transcript and footer (no per-chat tz).
7. **Process-wide budget** (40/h) can throttle legit multi-chat use.

## Change status (2026-09-25)

The owner gate is a **local, unpushed** change on branch
`feat/local-youtube-shorts-owner-summarize` (worktree
`.worktrees/bidlobot/feat/local-youtube-shorts-owner-summarize`); the
desktop unit still runs the previous build. Build/vet/test/docs
validation is planned for after the batch integrates - see
[handoff.md](handoff.md) and
[devlog/12_owner_summarize_and_shorts.md](devlog/12_owner_summarize_and_shorts.md).
