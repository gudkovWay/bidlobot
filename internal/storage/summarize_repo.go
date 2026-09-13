package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/veschin/bidlobot/internal/domain/summarize"
)

var bktSummarizeMsgs = []byte("summarize_msgs")

// summarizeRow is the on-disk shape of one persisted window row. The
// chat id lives in the key, so the value does not repeat it.
type summarizeRow struct {
	MsgID  int       `json:"msg_id"`
	UserID int64     `json:"user_id"`
	Name   string    `json:"name"`
	TS     time.Time `json:"ts"`
	Text   string    `json:"text"`
}

// SummarizeRepo persists the /summarize RAM window in its OWN bbolt
// file (DB_PATH/summarize.db), deliberately separate from bidlobot.db:
// the backup job copies the main database whole, and raw member text
// must not multiply into those snapshots.
type SummarizeRepo struct {
	db *bolt.DB
}

func NewSummarizeRepo(path string) (*SummarizeRepo, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open summarize db: %w", err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bktSummarizeMsgs)
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init summarize bucket: %w", err)
	}

	return &SummarizeRepo{db: db}, nil
}

func (r *SummarizeRepo) DB() *bolt.DB {
	return r.db
}

func (r *SummarizeRepo) Close() error {
	return r.db.Close()
}

// AppendRows writes a batch of rows in ONE update txn - one fsync per
// batch, which is the entire point of the caller-side queue. Rows are
// marshalled before the txn opens so a marshal error cannot abort a
// partially written batch.
func (r *SummarizeRepo) AppendRows(ctx context.Context, rows []summarize.Row) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	data := make([][]byte, len(rows))
	for i, row := range rows {
		b, err := json.Marshal(summarizeRow{
			MsgID:  row.MsgID,
			UserID: row.UserID,
			Name:   row.Name,
			TS:     row.TS,
			Text:   row.Text,
		})
		if err != nil {
			return fmt.Errorf("marshal summarize row: %w", err)
		}
		data[i] = b
	}

	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktSummarizeMsgs)
		for i, row := range rows {
			seq, err := b.NextSequence()
			if err != nil {
				return fmt.Errorf("next sequence: %w", err)
			}
			if err := b.Put(SummarizeMsgKey(row.AbsChatID, seq), data[i]); err != nil {
				return fmt.Errorf("put summarize row: %w", err)
			}
		}
		return nil
	})
}

// LoadRecent returns every persisted row with TS at or after cutoff,
// in cursor order (per-chat append order, thanks to the key layout).
// Corrupt or empty rows are skipped, not fatal: a lost row beats a
// lost restore.
func (r *SummarizeRepo) LoadRecent(ctx context.Context, cutoff time.Time) ([]summarize.Row, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var rows []summarize.Row
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktSummarizeMsgs)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var sr summarizeRow
			if err := json.Unmarshal(v, &sr); err != nil {
				return nil
			}
			if sr.TS.Before(cutoff) || sr.Text == "" {
				return nil
			}
			// "sm:" + 20-digit chat id: the id sits at a fixed
			// offset in every key this repo writes.
			rows = append(rows, summarize.Row{
				AbsChatID: parseID(k[len("sm:") : len("sm:")+20]),
				Entry: summarize.Entry{
					MsgID:  sr.MsgID,
					UserID: sr.UserID,
					Name:   sr.Name,
					TS:     sr.TS,
					Text:   sr.Text,
				},
			})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Prune enforces the RAM buffer's caps on disk and drops rows older
// than cutoff, in one update txn. Zero or negative caps mean "no cap"
// for that dimension. Returns the number of rows removed.
func (r *SummarizeRepo) Prune(ctx context.Context, cutoff time.Time, maxPerChat, maxBytesChat, maxChats int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	type liveRow struct {
		key    []byte
		chatID int64
		seq    uint64
		ts     time.Time
		bytes  int
	}

	removed := 0
	err := r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktSummarizeMsgs)
		if b == nil {
			return nil
		}

		// Single cursor pass: collect survivors (with the key
		// copies bbolt requires) and every row to drop. Deletes
		// happen after the pass - no mutation while iterating.
		var live []liveRow
		var expired [][]byte
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var sr summarizeRow
			if err := json.Unmarshal(v, &sr); err != nil || sr.TS.Before(cutoff) {
				expired = append(expired, append([]byte(nil), k...))
				continue
			}
			// Key layout: "sm:" (3) + 20-digit chat id + ":" (1),
			// so the id is a fixed slice and the seq runs to the end.
			live = append(live, liveRow{
				key:    append([]byte(nil), k...),
				chatID: parseID(k[len("sm:") : len("sm:")+20]),
				seq:    uint64(parseID(k[len("sm:")+21:])),
				ts:     sr.TS,
				bytes:  len(sr.Text),
			})
		}

		for _, k := range expired {
			if err := b.Delete(k); err != nil {
				return err
			}
			removed++
		}

		// Group survivors per chat, oldest (lowest seq) first.
		byChat := make(map[int64][]liveRow)
		for _, row := range live {
			byChat[row.chatID] = append(byChat[row.chatID], row)
		}
		for chatID := range byChat {
			rows := byChat[chatID]
			sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })

			var drop []liveRow
			// Count cap: keep the newest maxPerChat rows.
			if maxPerChat > 0 && len(rows) > maxPerChat {
				drop = append(drop, rows[:len(rows)-maxPerChat]...)
				rows = rows[len(rows)-maxPerChat:]
			}
			// Byte cap: drop oldest rows until the total fits, but
			// never the last remaining row - a single oversized
			// message is still kept (same rule as the RAM buffer).
			if maxBytesChat > 0 {
				total := 0
				for _, row := range rows {
					total += row.bytes
				}
				for total > maxBytesChat && len(rows) > 1 {
					total -= rows[0].bytes
					drop = append(drop, rows[0])
					rows = rows[1:]
				}
			}
			byChat[chatID] = rows

			for _, row := range drop {
				if err := b.Delete(row.key); err != nil {
					return err
				}
				removed++
			}
		}

		// Chat cap: drop whole chats, least-recently-active first
		// (activity = the chat's newest TS, mirroring the RAM
		// buffer's LRU by last record).
		if maxChats > 0 && len(byChat) > maxChats {
			type chatAge struct {
				chatID int64
				newest time.Time
				rows   []liveRow
			}
			ages := make([]chatAge, 0, len(byChat))
			for chatID, rows := range byChat {
				newest := rows[len(rows)-1].ts
				for _, row := range rows {
					if row.ts.After(newest) {
						newest = row.ts
					}
				}
				ages = append(ages, chatAge{chatID: chatID, newest: newest, rows: rows})
			}
			sort.Slice(ages, func(i, j int) bool { return ages[i].newest.Before(ages[j].newest) })
			for _, age := range ages[:len(ages)-maxChats] {
				for _, row := range age.rows {
					if err := b.Delete(row.key); err != nil {
						return err
					}
					removed++
				}
				delete(byChat, age.chatID)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
