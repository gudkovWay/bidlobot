package summarize

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultPersistTTL    = 7 * 24 * time.Hour
	defaultFlushInterval = 5 * time.Second
	defaultPruneInterval = 10 * time.Minute
	defaultQueueLimit    = 500
)

// Row is one persisted window entry: the chat id plus the Entry fields.
type Row struct {
	AbsChatID int64
	Entry
}

// Store is the persistence seam for the window. Implemented by
// storage.SummarizeRepo, which lives in its own bbolt file so member
// text never enters the main database or its backups.
type Store interface {
	AppendRows(ctx context.Context, rows []Row) error
	LoadRecent(ctx context.Context, cutoff time.Time) ([]Row, error)
	Prune(ctx context.Context, cutoff time.Time, maxPerChat, maxBytesChat, maxChats int) (int, error)
}

// PersistConfig bounds the durable window. Zero values fall back to the
// defaults above (same normalization style as NewBuffer/NewService).
type PersistConfig struct {
	TTL           time.Duration // entries older than this are pruned (default 168h)
	FlushInterval time.Duration // batched write cadence (default 5s)
	PruneInterval time.Duration // sweep cadence (default 10m)
	QueueLimit    int           // queued rows that force a synchronous flush (default 500)
}

// Persister batches window rows in RAM and writes them to the Store in
// one transaction per flush, mirroring stats.Buffer: the recorder runs on
// the update loop and a bbolt commit fsyncs, so a per-message write would
// stall every member's update.
type Persister struct {
	store Store
	buf   *Buffer
	log   *slog.Logger

	ttl           time.Duration
	flushInterval time.Duration
	pruneInterval time.Duration
	queueLimit    int

	mu    sync.Mutex
	queue []Row
}

// NewPersister builds a Persister, applying defaults for any zero config
// field.
func NewPersister(store Store, buf *Buffer, cfg PersistConfig, log *slog.Logger) *Persister {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultPersistTTL
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultFlushInterval
	}
	if cfg.PruneInterval <= 0 {
		cfg.PruneInterval = defaultPruneInterval
	}
	if cfg.QueueLimit <= 0 {
		cfg.QueueLimit = defaultQueueLimit
	}
	return &Persister{
		store:         store,
		buf:           buf,
		log:           log,
		ttl:           cfg.TTL,
		flushInterval: cfg.FlushInterval,
		pruneInterval: cfg.PruneInterval,
		queueLimit:    cfg.QueueLimit,
	}
}

// Append queues one row for the next flush. Cheap and non-blocking
// unless the queue reached QueueLimit, in which case it flushes
// synchronously so a burst cannot grow RAM unbounded.
func (p *Persister) Append(absChatID int64, e Entry) {
	p.mu.Lock()
	p.queue = append(p.queue, Row{AbsChatID: absChatID, Entry: e})
	overflow := len(p.queue) >= p.queueLimit
	p.mu.Unlock()
	if overflow {
		p.Flush()
	}
}

// Restore hydrates the RAM window from the store: rows newer than
// now-TTL are grouped per chat and seeded into the buffer. A failure is
// returned (the caller logs it and keeps running without history - a
// corrupt or unreadable window must NOT be fatal).
func (p *Persister) Restore(ctx context.Context) error {
	cutoff := time.Now().Add(-p.ttl)
	rows, err := p.store.LoadRecent(ctx, cutoff)
	if err != nil {
		return err
	}
	perChat := make(map[int64][]Entry)
	for _, r := range rows {
		if r.Text == "" {
			continue
		}
		perChat[r.AbsChatID] = append(perChat[r.AbsChatID], r.Entry)
	}
	for chatID, entries := range perChat {
		p.buf.Seed(chatID, entries)
	}
	p.log.Info("summarize window restored",
		"chats", len(perChat),
		"rows", len(rows),
		"cutoff", cutoff,
	)
	return nil
}

// Run blocks until ctx is done, flushing every FlushInterval and pruning
// every PruneInterval; a final flush runs before returning.
func (p *Persister) Run(ctx context.Context) {
	flusher := time.NewTicker(p.flushInterval)
	defer flusher.Stop()
	pruner := time.NewTicker(p.pruneInterval)
	defer pruner.Stop()
	for {
		select {
		case <-flusher.C:
			p.Flush()
		case <-pruner.C:
			p.prune(ctx)
		case <-ctx.Done():
			p.Flush()
			return
		}
	}
}

// prune runs the periodic disk sweep: drop entries past the TTL and
// enforce the same per-chat / per-chat-count caps the ring holds. Quiet
// unless something was actually removed.
func (p *Persister) prune(ctx context.Context) {
	maxPerChat, maxBytesChat, maxChats := p.buf.Caps()
	removed, err := p.store.Prune(ctx, time.Now().Add(-p.ttl), maxPerChat, maxBytesChat, maxChats)
	if err != nil {
		p.log.Warn("summarize prune failed", "error", err)
		return
	}
	if removed > 0 {
		p.log.Info("summarize prune removed rows", "removed", removed)
	}
}

// Flush drains the queue into the store in one AppendRows call. On
// error the rows are re-queued in order so nothing is silently lost;
// the next tick retries. Safe to call concurrently with Run.
func (p *Persister) Flush() {
	p.mu.Lock()
	if len(p.queue) == 0 {
		p.mu.Unlock()
		return
	}
	toFlush := p.queue
	p.queue = nil
	p.mu.Unlock()

	if err := p.store.AppendRows(context.Background(), toFlush); err != nil {
		p.mu.Lock()
		p.queue = append(toFlush, p.queue...)
		p.mu.Unlock()
		p.log.Warn("summarize persist flush failed", "rows", len(toFlush), "error", err)
	}
}
