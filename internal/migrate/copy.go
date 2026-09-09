package migrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// copyTable moves one table's rows.
//
// The column list comes from the target, intersected with the source. Not from
// a list in this file: the schema is already defined twice and a third
// definition here would drift from both. Reading it also makes the copier
// survive a column added to one side and not the other — it copies what both
// have and says how many it moved, rather than failing on a name it does not
// recognise.
func copyTable(ctx context.Context, src, dst *storage.DB, table string, opts Options) (int64, int64, error) {
	srcCols, err := storage.Columns(src, table)
	if err != nil {
		return 0, 0, fmt.Errorf("source columns: %w", err)
	}
	if len(srcCols) == 0 {
		// Nothing to copy and nothing wrong: a SQLite deployment that never
		// enabled a feature has no table for it.
		return 0, 0, nil
	}
	dstTypes, err := storage.ColumnTypes(dst, table)
	if err != nil {
		return 0, 0, fmt.Errorf("target columns: %w", err)
	}
	if len(dstTypes) == 0 {
		return 0, 0, fmt.Errorf("目标库没有这张表 —— 先跑 migrations/postgres/0001_init.sql，"+
			"ADK 的 sessions/events/app_states/user_states 由程序启动时 AutoMigrate 建（表: %s）", table)
	}

	cols := make([]string, 0, len(srcCols))
	var dropped []string
	for _, c := range srcCols {
		if _, ok := dstTypes[c]; ok {
			cols = append(cols, c)
			continue
		}
		dropped = append(dropped, c)
	}
	if len(cols) == 0 {
		return 0, 0, fmt.Errorf("两边没有共同的列")
	}
	if len(dropped) > 0 && !opts.AllowDroppingColumns {
		// Refused, not skipped.
		//
		// Copying the other columns and saying nothing is silent data loss on
		// the one operation nobody re-runs to check: the source is about to
		// stop being read, and the value in that column is then gone. The two
		// schema definitions being one commit apart is a real situation, and
		// the answer to it is to say so and let somebody decide — usually by
		// adding the column to migrations/postgres/0001_init.sql, which takes
		// a minute, rather than by discovering it a week later.
		return 0, 0, &DroppedColumnsError{Table: table, Columns: dropped}
	}

	quoted := strings.Join(cols, ",")
	rows, err := src.QueryContext(ctx, `SELECT `+quoted+` FROM `+table)
	if err != nil {
		return 0, 0, fmt.Errorf("read: %w", err)
	}
	defer rows.Close()

	var copied, skipped int64
	batch := make([][]any, 0, opts.BatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := insertBatch(ctx, dst, table, cols, batch)
		if err != nil {
			return err
		}
		copied += n
		skipped += int64(len(batch)) - n
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return copied, skipped, fmt.Errorf("scan: %w", err)
		}
		for i, c := range cols {
			vals[i] = coerce(vals[i], dstTypes[c])
		}
		if opts.DryRun {
			copied++
			continue
		}
		batch = append(batch, vals)
		if len(batch) >= opts.BatchSize {
			if err := flush(); err != nil {
				return copied, skipped, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return copied, skipped, err
	}
	if opts.DryRun {
		return copied, 0, nil
	}
	return copied, skipped, flush()
}

// insertBatch writes one batch and reports how many rows were new.
//
// ON CONFLICT DO NOTHING on the table's own primary key, so an interrupted run
// can be started again: the rows already there are skipped rather than failing
// the copy. That also makes the count meaningful — "copied" is rows this run
// added, not rows it looked at.
func insertBatch(ctx context.Context, dst *storage.DB, table string, cols []string, batch [][]any) (int64, error) {
	var b strings.Builder
	b.WriteString("INSERT INTO " + table + " (" + strings.Join(cols, ",") + ") VALUES ")
	args := make([]any, 0, len(batch)*len(cols))
	for i, row := range batch {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		b.WriteString(storage.Placeholders(len(cols)))
		b.WriteByte(')')
		args = append(args, row...)
	}
	b.WriteString(" ON CONFLICT DO NOTHING")

	res, err := dst.ExecContext(ctx, b.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("write %d rows: %w", len(batch), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The write landed; only the count is unavailable. Reporting the batch
		// size is closer to the truth than reporting zero.
		return int64(len(batch)), nil
	}
	return n, nil
}
