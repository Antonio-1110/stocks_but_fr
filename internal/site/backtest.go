package site

import (
	"database/sql"
	"fmt"
	"strconv"
)

// Field is one labelled value from a backtest run.
type Field struct {
	Name, Value string
}

// loadBacktest returns the newest row of the backtest_runs table that the
// Python engine (#17) writes, as label/value pairs. The engine owns that
// schema, so this reads whatever columns exist; nil means no run yet.
func loadBacktest(db *sql.DB) ([]Field, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'backtest_runs'`).Scan(&n); err != nil || n == 0 {
		return nil, err
	}
	q, err := db.Query(`SELECT * FROM backtest_runs ORDER BY rowid DESC LIMIT 1`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	cols, err := q.Columns()
	if err != nil || !q.Next() {
		return nil, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := q.Scan(ptrs...); err != nil {
		return nil, err
	}
	var out []Field
	for i, c := range cols {
		out = append(out, Field{Name: c, Value: formatAny(vals[i])})
	}
	return out, q.Err()
}

func formatAny(v any) string {
	switch x := v.(type) {
	case nil:
		return "–"
	case []byte:
		return string(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}
