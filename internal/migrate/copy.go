package migrate

import (
	"context"
	"database/sql"
	"errors"
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
		if opts.DryRun {
			// A dry run is asked before the target is ready — that is what it
			// is for. Reporting the count it would copy is more useful than
			// refusing to look, and it writes nothing either way.
			n, err := countRows(ctx, src, table)
			return n, 0, err
		}
		return 0, 0, fmt.Errorf("目标库没有这张表 —— 先跑 migrations/postgres/0001_init.sql，"+
			"ADK 的 sessions/events/app_states/user_states 由程序启动时 AutoMigrate 建（表: %s）", table)
	}

	cols := make([]string, 0, len(srcCols))
	var dropped, empty []string
	for _, c := range srcCols {
		if _, ok := dstTypes[c]; ok {
			cols = append(cols, c)
			continue
		}
		// Only a column that actually holds something. tool_results carries a
		// vestigial `label` from a schema nobody writes any more — every row
		// has the empty string in it — and refusing to migrate over a column
		// with nothing in it would stop every real upgrade for no reason.
		//
		// Checked rather than listed: a hardcoded exception would be a fourth
		// place to describe the schema, and it would go stale.
		has, err := columnHasValues(ctx, src, table, c)
		if err != nil {
			return 0, 0, err
		}
		if has {
			dropped = append(dropped, c)
		} else {
			empty = append(empty, c)
		}
	}
	if len(cols) == 0 {
		return 0, 0, fmt.Errorf("两边没有共同的列")
	}
	if len(empty) > 0 {
		// Worth saying: a column the target does not have is a schema
		// difference somebody may want to know about, even when nothing is
		// lost by ignoring it.
		opts.Note(fmt.Sprintf("%s 跳过了目标库没有的空列 %s（源库里这些列没有任何值）",
			table, strings.Join(empty, ", ")))
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

	// The primary key, so the copier can say *which* rows the target already
	// had. Without it a skipped row is only a number, and a number is exactly
	// what cannot tell "already copied, identical" from "the target holds
	// something else under this key".
	pk, err := storage.PrimaryKey(dst, table)
	if err != nil {
		return 0, 0, fmt.Errorf("target primary key: %w", err)
	}
	keyIdx := keyIndexes(cols, pk)
	if !opts.DryRun && keyIdx == nil {
		// Said out loud rather than passed over: the rows still copy, but a
		// skip in this table cannot be checked, and a reader deserves to know
		// which of the two kinds of "已存在" they are looking at.
		opts.Note(fmt.Sprintf("%s 没有可用的主键，跳过的行只能计数、无法核对内容", table))
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
		n, missing, err := insertBatch(ctx, dst, table, cols, batch, pk, keyIdx)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			bad, total, err := compareExisting(ctx, dst, table, cols, missing, pk, keyIdx)
			if err != nil {
				return err
			}
			if total > 0 {
				return &ConflictError{Table: table, Conflicts: bad, Total: total}
			}
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

// insertBatch writes one batch, and reports how many rows were new plus the
// rows the target already had.
//
// ON CONFLICT DO NOTHING on any of the table's constraints, so an interrupted
// run can be started again: the rows already there are skipped rather than
// failing the copy. That also makes the count meaningful — "copied" is rows
// this run added, not rows it looked at.
//
// RETURNING the primary key is what turns that count into an answer. Only
// inserted rows come back, so whatever is missing from the returned set is
// what the target already had, named — and named is the difference between
// reporting a number and being able to check it (see compareExisting). A
// table whose key is not among the copied columns has no such answer and
// falls back to the count alone.
func insertBatch(ctx context.Context, dst *storage.DB, table string, cols []string, batch [][]any, pk []string, keyIdx []int) (int64, [][]any, error) {
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

	if keyIdx == nil {
		res, err := dst.ExecContext(ctx, b.String(), args...)
		if err != nil {
			return 0, nil, fmt.Errorf("write %d rows: %w", len(batch), err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			// The write landed; only the count is unavailable. Reporting the
			// batch size is closer to the truth than reporting zero.
			return int64(len(batch)), nil, nil
		}
		return n, nil, nil
	}

	b.WriteString(" RETURNING " + strings.Join(pk, ","))
	rows, err := dst.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return 0, nil, fmt.Errorf("write %d rows: %w", len(batch), err)
	}
	added := map[string]bool{}
	for rows.Next() {
		vals := make([]any, len(pk))
		ptrs := make([]any, len(pk))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return 0, nil, err
		}
		key := make([]string, len(vals))
		for i, v := range vals {
			key[i] = canon(v)
		}
		added[strings.Join(key, ",")] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, nil, fmt.Errorf("write %d rows: %w", len(batch), err)
	}
	rows.Close()

	var missing [][]any
	for _, row := range batch {
		if !added[keyOf(row, keyIdx)] {
			missing = append(missing, row)
		}
	}
	return int64(len(added)), missing, nil
}

// keyIndexes locates the primary-key columns among the ones being copied, or
// nil when the key is unusable — no key at all, or a key column the copy is
// not carrying.
func keyIndexes(cols, pk []string) []int {
	if len(pk) == 0 {
		return nil
	}
	at := make(map[string]int, len(cols))
	for i, c := range cols {
		at[c] = i
	}
	idx := make([]int, 0, len(pk))
	for _, k := range pk {
		i, ok := at[k]
		if !ok {
			return nil
		}
		idx = append(idx, i)
	}
	return idx
}

// columnHasValues reports whether any row has something in this column.
//
// "Something" excludes NULL and the empty string: a column left at its zero
// value by every row carries no information, and treating it as data to
// preserve would block a migration over a schema difference that costs
// nothing.
func columnHasValues(ctx context.Context, db *storage.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM `+table+` WHERE `+column+` IS NOT NULL AND `+column+` <> ''`).Scan(&n)
	if err != nil {
		// A type that cannot be compared to '' (a blob, a number) is a column
		// with real content as far as this is concerned.
		return true, nil //nolint:nilerr // see above
	}
	return n > 0, nil
}

// advanceIdentities moves each identity column past the ids that were copied.
//
// Rows arrive with their ids, so the target's sequence never advanced — and
// the first row the service writes after a migration asks for id 1, which is
// already taken. Demonstrated: three rows copied, then a plain insert fails
// with duplicate key on the primary key. The migration reports success and
// the deployment cannot write.
//
// Done after the copy rather than during it, because a batch can be retried
// and the sequence only has to end up past the highest id that exists.
//
// SQLite needs none of this: AUTOINCREMENT reads MAX(rowid) rather than a
// separate counter, so a copied row moves it by existing.
func advanceIdentities(ctx context.Context, dst *storage.DB, tables []string) error {
	if !dst.HasIdentitySequences() {
		return nil
	}
	for _, table := range tables {
		col, err := identityColumn(ctx, dst, table)
		if err != nil {
			return err
		}
		if col == "" {
			continue // no generated column; nothing to advance
		}
		// setval with is_called=false so the next value is exactly max+1,
		// and COALESCE so an empty table starts at 1 rather than at 0.
		if _, err := dst.ExecContext(ctx, fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%s', '%s'),
			                COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)`,
			table, col, col, table)); err != nil {
			return fmt.Errorf("migrate: advance %s.%s: %w", table, col, err)
		}
	}
	return nil
}

// identityColumn names the table's generated column, or empty.
func identityColumn(ctx context.Context, db *storage.DB, table string) (string, error) {
	var col *string
	err := db.QueryRowContext(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ?
		  AND (is_identity = 'YES' OR column_default LIKE 'nextval%')
		LIMIT 1`, table).Scan(&col)
	if err != nil {
		if storage.IsMissingTable(err) || isNoRows(err) {
			return "", nil
		}
		return "", fmt.Errorf("migrate: find identity of %s: %w", table, err)
	}
	if col == nil {
		return "", nil
	}
	return *col, nil
}

// isNoRows is sql.ErrNoRows, reached through the package that owns the native
// types so this one does not name them.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
