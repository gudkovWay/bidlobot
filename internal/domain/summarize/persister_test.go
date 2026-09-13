package summarize

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// fakeStore is an in-memory Store: rows land in the slice once
// AppendRows succeeds.
type fakeStore struct {
	rows []Row
	fail int // remaining failed AppendRows calls
}

func (f *fakeStore) AppendRows(_ context.Context, rows []Row) error {
	if f.fail > 0 {
		f.fail--
		return errors.New("boom")
	}
	f.rows = append(f.rows, rows...)
	return nil
}

func (f *fakeStore) LoadRecent(_ context.Context, cutoff time.Time) ([]Row, error) {
	var out []Row
	for _, r := range f.rows {
		if r.TS.After(cutoff) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) Prune(_ context.Context, _ time.Time, _, _, _ int) (int, error) {
	return 0, nil
}

func entryAt(min int, text string) Entry {
	return Entry{MsgID: min, TS: time.Unix(int64(1700000000+min*60), 0), Text: text}
}

// freshEntryAt is entryAt with distinct timestamps inside the default
// 7-day restore TTL, so LoadRecent returns them all.
func freshEntryAt(min int, text string) Entry {
	return Entry{MsgID: min, TS: time.Now().Add(-time.Duration(100-min) * time.Minute), Text: text}
}

func TestAppendFlushDeliversRowsInOrder(t *testing.T) {
	fs := &fakeStore{}
	svc := NewService(NewBuffer(BufferConfig{}), nil, Config{}, slog.New(slog.DiscardHandler))
	svc.EnablePersistence(fs, PersistConfig{})

	for i := 0; i < 3; i++ {
		svc.Record(100, entryAt(i, "t"))
	}
	svc.FlushPersistence()

	if len(fs.rows) != 3 {
		t.Fatalf("store has %d rows, want 3", len(fs.rows))
	}
	for i, r := range fs.rows {
		if r.AbsChatID != 100 || r.MsgID != i {
			t.Fatalf("row %d = chat %d msg %d, want chat 100 msg %d", i, r.AbsChatID, r.MsgID, i)
		}
	}
}

func TestQueueLimitForcesFlush(t *testing.T) {
	fs := &fakeStore{}
	p := NewPersister(fs, NewBuffer(BufferConfig{}), PersistConfig{QueueLimit: 5}, slog.New(slog.DiscardHandler))
	for i := 0; i < 5; i++ {
		p.Append(7, entryAt(i, "x"))
	}
	if len(fs.rows) != 5 {
		t.Fatalf("store has %d rows after QueueLimit append, want 5 (no explicit Flush)", len(fs.rows))
	}
}

func TestFlushErrorRequeuesAndRetries(t *testing.T) {
	fs := &fakeStore{fail: 1}
	svc := NewService(NewBuffer(BufferConfig{}), nil, Config{}, slog.New(slog.DiscardHandler))
	svc.EnablePersistence(fs, PersistConfig{})

	for i := 0; i < 4; i++ {
		svc.Record(100, entryAt(i, "t"))
	}
	svc.FlushPersistence() // fails, rows re-queued
	if len(fs.rows) != 0 {
		t.Fatalf("store has %d rows after failed flush, want 0", len(fs.rows))
	}
	svc.FlushPersistence() // succeeds

	if len(fs.rows) != 4 {
		t.Fatalf("store has %d rows, want 4 (each delivered exactly once)", len(fs.rows))
	}
	seen := map[int]bool{}
	for _, r := range fs.rows {
		if seen[r.MsgID] {
			t.Fatalf("row msg %d delivered twice", r.MsgID)
		}
		seen[r.MsgID] = true
	}
}

func TestRestoreThroughService(t *testing.T) {
	fs := &fakeStore{rows: []Row{
		{AbsChatID: 42, Entry: freshEntryAt(0, "a")},
		{AbsChatID: 42, Entry: freshEntryAt(1, "b")},
		{AbsChatID: 42, Entry: freshEntryAt(2, "c")},
	}}
	svc := NewService(NewBuffer(BufferConfig{}), nil, Config{}, slog.New(slog.DiscardHandler))
	svc.EnablePersistence(fs, PersistConfig{})
	svc.RestorePersistence(context.Background())

	if got := svc.Available(42); got != 3 {
		t.Fatalf("Available = %d, want 3", got)
	}
	win, _ := svc.buf.Window(42, 0)
	if len(win) != 3 || win[0].Text != "a" || win[2].Text != "c" {
		t.Fatalf("Window oldest-first broken: %+v", win)
	}
}

func TestSeedRespectsCountCap(t *testing.T) {
	b := NewBuffer(BufferConfig{MaxPerChat: 2})
	entries := []Entry{entryAt(0, "e0"), entryAt(1, "e1"), entryAt(2, "e2"), entryAt(3, "e3"), entryAt(4, "e4")}
	b.Seed(9, entries)

	win, total := b.Window(9, 0)
	if total != 2 || len(win) != 2 {
		t.Fatalf("Window = %d rows (total %d), want 2", len(win), total)
	}
	if win[0].Text != "e3" || win[1].Text != "e4" {
		t.Fatalf("Window = [%s %s], want newest two [e3 e4]", win[0].Text, win[1].Text)
	}
}

func TestRunFlushesOnTickAndOnShutdown(t *testing.T) {
	fs := &fakeStore{}
	buf := NewBuffer(BufferConfig{})
	p := NewPersister(fs, buf, PersistConfig{FlushInterval: 20 * time.Millisecond, PruneInterval: time.Hour}, slog.New(slog.DiscardHandler))

	p.Append(100, entryAt(1, "tick"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for len(fs.rows) == 0 {
		select {
		case <-deadline:
			t.Fatalf("store received no batch within 2s of ticking flushes")
		case <-time.After(time.Millisecond):
		}
	}

	p.Append(100, entryAt(2, "shutdown"))
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s of context cancellation")
	}
	if len(fs.rows) != 2 {
		t.Fatalf("store has %d rows after final flush, want 2", len(fs.rows))
	}
	if fs.rows[1].MsgID != 2 || fs.rows[1].Text != "shutdown" {
		t.Fatalf("row appended before cancellation missing from store: %+v", fs.rows[1])
	}
}
