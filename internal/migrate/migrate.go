// Package migrate copies a jelly-agent state database into another one.
//
// In practice that means SQLite → PostgreSQL, which is the move the rest of
// this work exists for. It is written as a copy between two storage.DB handles
// rather than as a PostgreSQL script, for three reasons:
//
//   - The type differences are real. SQLite keeps booleans as 0 and 1 and
//     timestamps as text; PostgreSQL has both types. A dump-and-load would
//     need the same coercions written a second time, in SQL.
//   - The schema is already defined twice — each store's DDL and
//     migrations/postgres/0001_init.sql. A copier carrying its own column list
//     would be a third, and would drift from both. This one reads what the
//     target actually has.
//   - Verification belongs with the copy. A migration nobody can check is a
//     migration nobody can trust.
package migrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// Tables are copied in this order, and the order matters.
//
// ADK's four come first because events reference sessions; the rest are keyed
// by (session_id, invocation_id) with no constraint between them, but copying
// them after keeps a partially finished run from showing evidence for a
// conversation that is not there yet.
//
// memory_fts is deliberately absent. It is a derived index — rebuilt from
// events on the next turn of each session — so copying it would move stale
// rows that the first write replaces anyway. See Report.Note.
// tool_result_seq is copied for exactness rather than for correctness. A
// target without it is reseeded by record.Open, which takes MAX(seq) per
// session — so handles still never collide, the counter just loses the numbers
// a re-delivery had burned and the gaps close up. Copying it keeps the
// migrated database byte-for-byte where the original was; not copying it would
// also be safe, and no test can tell the two apart because both are correct.
var Tables = []string{
	"sessions",
	"events",
	"app_states",
	"user_states",
	"tool_results",
	"tool_result_seq",
	"tool_calls",
	"task_runs",
	"schedule_runs",
}

// Report is what one run copied, per table.
type Report struct {
	Copied  map[string]int64
	Skipped map[string]int64 // rows the target already had
	Order   []string
	Note    string
}

// DroppedColumnsError says the source has columns the target does not.
//
// Its own type so a caller can tell this apart from a failure — it is not that
// the copy went wrong, it is that finishing it would lose data, and the fix is
// usually one line in the migration file.
type DroppedColumnsError struct {
	Table   string
	Columns []string
}

func (e *DroppedColumnsError) Error() string {
	return fmt.Sprintf("%s 有目标库没有的列 %s —— 照搬会丢掉这些列的值。"+
		"把它们加进 migrations/postgres/0001_init.sql 再跑；"+
		"确实不要这些值，用 --allow-dropping-columns",
		e.Table, strings.Join(e.Columns, ", "))
}

// Options bound one run.
type Options struct {
	// BatchSize is how many rows go in one INSERT. Zero takes the default.
	BatchSize int
	// DryRun counts what would be copied without writing anything.
	DryRun bool
	// AllowDroppingColumns copies what both sides have and discards the rest,
	// instead of refusing. For the case where somebody has looked at the list
	// and decided those values are not worth keeping.
	AllowDroppingColumns bool
	// Progress, when set, is called after each table.
	Progress func(table string, copied, skipped int64)
}

const defaultBatch = 200

// Run copies every table from src into dst.
//
// Both must already have their schema: the source because it is a live
// database, the target because on PostgreSQL the schema belongs to
// migrations/postgres/0001_init.sql and ADK's four tables to its own
// AutoMigrate. Run does not create anything, so a missing table is reported
// rather than papered over.
//
// Re-runnable. Every insert is ON CONFLICT DO NOTHING on the table's own
// primary key, so a run interrupted halfway can simply be started again — the
// rows already there are counted as skipped rather than failing the copy.
func Run(ctx context.Context, src, dst *storage.DB, opts Options) (Report, error) {
	rep := Report{Copied: map[string]int64{}, Skipped: map[string]int64{}}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatch
	}
	rep.Note = "memory_fts 未迁移：它是从 events 重建的派生索引，" +
		"每个会话的下一轮对话会自动重建。"

	for _, table := range Tables {
		copied, skipped, err := copyTable(ctx, src, dst, table, opts)
		if err != nil {
			return rep, fmt.Errorf("migrate %s: %w", table, err)
		}
		rep.Order = append(rep.Order, table)
		rep.Copied[table] = copied
		rep.Skipped[table] = skipped
		if opts.Progress != nil {
			opts.Progress(table, copied, skipped)
		}
	}
	return rep, nil
}
