package alerts

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/Antonio-1110/stocks_but_fr/internal/config"
	"github.com/Antonio-1110/stocks_but_fr/internal/store"
)

// maxAge: a message not delivered within this long is marked expired rather
// than sent late, e.g. after Telegram was down for days.
const maxAge = 72 * time.Hour

// Notify is the `radar notify` command: it sends the messages `alerts`
// queued, oldest first, and marks each one handled. The workflow runs it only
// after the state database is saved, so a failed save never leaves sent
// alerts to be sent again (issue #47).
func Notify(ctx context.Context, cfg config.Config, st *store.Store) error {
	var s sender
	if t := telegramFromEnv(); t != nil {
		s = t
	} else {
		log.Printf("notify: TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID not set; logging alerts instead of sending")
	}
	return notify(ctx, st.DB, s, time.Now())
}

type queued struct {
	id      int64
	rule    string
	created time.Time
	body    string
}

func notify(ctx context.Context, db *sql.DB, s sender, now time.Time) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("alert tables: %w", err)
	}
	rows, err := db.Query(`SELECT id, rule, created_at, body FROM alert_outbox WHERE sent_at = '' ORDER BY id`)
	if err != nil {
		return err
	}
	var todo []queued
	for rows.Next() {
		var q queued
		var created string
		if err := rows.Scan(&q.id, &q.rule, &created, &q.body); err != nil {
			rows.Close()
			return err
		}
		q.created, _ = time.Parse(time.RFC3339, created)
		todo = append(todo, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	mark := func(id int64, via string) error {
		_, err := db.Exec(`UPDATE alert_outbox SET sent_at = ?, sent_via = ? WHERE id = ? AND sent_at = ''`,
			now.UTC().Format(time.RFC3339), via, id)
		return err
	}
	sent := 0
	for _, q := range todo {
		switch {
		case now.Sub(q.created) > maxAge:
			log.Printf("notify: %s: message %d from %s is too old; dropped", q.rule, q.id, q.created.Format(time.RFC3339))
			if err := mark(q.id, "expired"); err != nil {
				return err
			}
		case s == nil:
			log.Printf("notify: %s: would send:\n%s", q.rule, q.body)
			if err := mark(q.id, "log"); err != nil {
				return err
			}
		default:
			// Stop at the first failure: the rest stay queued, in order, for
			// the next run.
			if err := s.Send(ctx, q.body); err != nil {
				return fmt.Errorf("send message %d: %w (%d left queued)", q.id, err, len(todo)-sent)
			}
			if err := mark(q.id, "telegram"); err != nil {
				return err
			}
			sent++
		}
	}
	log.Printf("notify: %d queued, %d sent", len(todo), sent)
	return nil
}
