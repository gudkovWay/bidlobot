package storage

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/veschin/bidlobot/internal/domain/summarize"
)

func newTestSummarizeRepo(t *testing.T) *SummarizeRepo {
	t.Helper()
	repo, err := NewSummarizeRepo(filepath.Join(t.TempDir(), "summarize.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo
}

// sumRow builds a summarize.Row with a deterministic user id so the
// round-trip assertions can check every Entry field.
func sumRow(absChatID int64, msgID int, name, text string, ts time.Time) summarize.Row {
	return summarize.Row{
		AbsChatID: absChatID,
		Entry: summarize.Entry{
			MsgID:  msgID,
			UserID: int64(1000 + msgID),
			Name:   name,
			TS:     ts,
			Text:   text,
		},
	}
}

func sumText(i int) string { return "message number " + string(rune('0'+i)) }

func TestSummarizeRepo_AppendLoadRoundTrip(t *testing.T) {
	repo := newTestSummarizeRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Two chats, interleaved appends; one row is older than the cutoff.
	rows := []summarize.Row{
		sumRow(100, 1, "alice", "first of chat 100", base),
		sumRow(200, 1, "bob", "first of chat 200", base),
		sumRow(100, 2, "carol", "second of chat 100", base.Add(time.Minute)),
		sumRow(100, 3, "dave", "stale row of chat 100", base.Add(-time.Hour)),
	}
	if err := repo.AppendRows(ctx, rows); err != nil {
		t.Fatal(err)
	}

	got, err := repo.LoadRecent(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3 (stale row excluded)", len(got))
	}

	// Per-chat append order and full field round-trip. Cursor order is
	// chat-major, so chat 100's rows come first.
	wantTexts := []string{"first of chat 100", "second of chat 100", "first of chat 200"}
	wantChats := []int64{100, 100, 200}
	wantNames := []string{"alice", "carol", "bob"}
	wantMsgIDs := []int{1, 2, 1}
	for i, r := range got {
		if r.AbsChatID != wantChats[i] {
			t.Errorf("row %d chat = %d, want %d", i, r.AbsChatID, wantChats[i])
		}
		if r.Text != wantTexts[i] {
			t.Errorf("row %d text = %q, want %q", i, r.Text, wantTexts[i])
		}
		if r.Name != wantNames[i] {
			t.Errorf("row %d name = %q, want %q", i, r.Name, wantNames[i])
		}
		if r.MsgID != wantMsgIDs[i] {
			t.Errorf("row %d msg id = %d, want %d", i, r.MsgID, wantMsgIDs[i])
		}
		if r.UserID != int64(1000+wantMsgIDs[i]) {
			t.Errorf("row %d user id = %d, want %d", i, r.UserID, int64(1000+wantMsgIDs[i]))
		}
	}

	// Empty append is a no-op, not an error.
	if err := repo.AppendRows(ctx, nil); err != nil {
		t.Errorf("AppendRows(nil) = %v, want nil", err)
	}
}

func TestSummarizeRepo_PruneTTL(t *testing.T) {
	repo := newTestSummarizeRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	if err := repo.AppendRows(ctx, []summarize.Row{
		sumRow(100, 1, "a", "old one", base.Add(-2*time.Hour)),
		sumRow(100, 2, "b", "old two", base.Add(-time.Hour)),
		sumRow(100, 3, "c", "fresh one", base),
		sumRow(100, 4, "d", "fresh two", base.Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := repo.Prune(ctx, base, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	got, err := repo.LoadRecent(ctx, base.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("survivors = %d, want 2", len(got))
	}
	if got[0].Text != "fresh one" || got[1].Text != "fresh two" {
		t.Errorf("survivor texts = %q, %q", got[0].Text, got[1].Text)
	}
}

func TestSummarizeRepo_PrunePerChatCountCap(t *testing.T) {
	repo := newTestSummarizeRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	var rows []summarize.Row
	for i := 1; i <= 5; i++ {
		rows = append(rows, sumRow(100, i, "u", sumText(i), base.Add(time.Duration(i)*time.Minute)))
	}
	rows = append(rows, sumRow(200, 1, "v", "other chat", base))
	if err := repo.AppendRows(ctx, rows); err != nil {
		t.Fatal(err)
	}

	removed, err := repo.Prune(ctx, base.Add(-time.Hour), 2, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}

	got, err := repo.LoadRecent(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("survivors = %d, want 3", len(got))
	}
	// The 2 newest of chat 100 and chat 200's single row survive.
	want := []struct {
		chat int64
		text string
	}{{100, sumText(4)}, {100, sumText(5)}, {200, "other chat"}}
	for i, w := range want {
		if got[i].AbsChatID != w.chat || got[i].Text != w.text {
			t.Errorf("survivor %d = (chat %d, %q), want (chat %d, %q)", i, got[i].AbsChatID, got[i].Text, w.chat, w.text)
		}
	}
}

func TestSummarizeRepo_PruneChatCap(t *testing.T) {
	repo := newTestSummarizeRepo(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// Chat 300's newest message is the oldest of the three chats ->
	// the LRU drops chat 300 entirely.
	if err := repo.AppendRows(ctx, []summarize.Row{
		sumRow(100, 1, "a", "chat 100 msg", base.Add(3*time.Hour)),
		sumRow(200, 1, "b", "chat 200 msg", base.Add(2*time.Hour)),
		sumRow(300, 1, "c", "chat 300 msg", base.Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := repo.Prune(ctx, base.Add(-time.Hour), 0, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	got, err := repo.LoadRecent(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("survivors = %d, want 2", len(got))
	}
	for _, r := range got {
		if r.AbsChatID == 300 {
			t.Error("chat 300 should have been evicted entirely")
		}
	}
}

func TestSummarizeRepo_ReopenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summarize.db")
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	repo, err := NewSummarizeRepo(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendRows(ctx, []summarize.Row{
		sumRow(100, 1, "alice", "survives the restart", base),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	repo2, err := NewSummarizeRepo(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repo2.Close()

	got, err := repo2.LoadRecent(ctx, base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after reopen: got %d rows, want 1", len(got))
	}
	if got[0].AbsChatID != 100 || got[0].Text != "survives the restart" || got[0].Name != "alice" || got[0].MsgID != 1 {
		t.Errorf("after reopen: got %+v, want the original row", got[0])
	}
}

// TestSummarizeWindowSurvivesRestart is the end-to-end check of the
// feature's core claim: a window fed through the real domain service,
// flushed to a real bbolt file and reopened in a fresh "process",
// restores exactly, per chat, oldest-first, while a service without
// persistence stays empty.
func TestSummarizeWindowSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summarize.db")
	ctx := context.Background()
	base := time.Now().Add(-time.Hour) // well inside the default 168h TTL

	newSvc := func() *summarize.Service {
		return summarize.NewService(summarize.NewBuffer(summarize.BufferConfig{}), nil, summarize.Config{}, slog.New(slog.DiscardHandler))
	}

	// First "process": record and flush, then go away.
	repo1, err := NewSummarizeRepo(path)
	if err != nil {
		t.Fatal(err)
	}
	svc1 := newSvc()
	svc1.EnablePersistence(repo1, summarize.PersistConfig{})
	chatA, chatB := int64(300), int64(400)
	for _, c := range []struct {
		chat  int64
		texts []string
	}{{chatA, []string{"a-one", "a-two", "a-three"}}, {chatB, []string{"b-one", "b-two"}}} {
		for j, text := range c.texts {
			svc1.Record(c.chat, summarize.Entry{
				MsgID:  j + 1,
				UserID: int64(2000 + j),
				Name:   "user",
				TS:     base.Add(time.Duration(j) * time.Second),
				Text:   text,
			})
		}
	}
	svc1.FlushPersistence()
	if err := repo1.Close(); err != nil {
		t.Fatal(err)
	}

	// Second "process": same file, fresh service, restore.
	repo2, err := NewSummarizeRepo(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repo2.Close()
	svc2 := newSvc()
	svc2.EnablePersistence(repo2, summarize.PersistConfig{})
	svc2.RestorePersistence(ctx)

	if got := svc2.Available(chatA); got != 3 {
		t.Fatalf("chat A after restart: Available = %d, want 3", got)
	}
	if got := svc2.Available(chatB); got != 2 {
		t.Fatalf("chat B after restart: Available = %d, want 2", got)
	}

	rows, err := repo2.LoadRecent(ctx, base.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var gotTexts []string
	for _, r := range rows {
		if r.AbsChatID == chatA {
			gotTexts = append(gotTexts, r.Text)
		}
	}
	wantTexts := []string{"a-one", "a-two", "a-three"}
	if len(gotTexts) != len(wantTexts) {
		t.Fatalf("chat A rows after restart: got %d (%v), want %v", len(gotTexts), gotTexts, wantTexts)
	}
	for i := range wantTexts {
		if gotTexts[i] != wantTexts[i] {
			t.Fatalf("chat A row %d: Text = %q, want %q (rows must come back oldest-first with the original texts)", i, gotTexts[i], wantTexts[i])
		}
	}

	// Counter-example: a service with persistence never enabled must
	// see nothing — the restored window really comes from the store.
	svc3 := newSvc()
	if got := svc3.Available(chatA); got != 0 {
		t.Fatalf("RAM-only service: Available = %d, want 0", got)
	}
}
