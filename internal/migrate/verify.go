package migrate

import (
	"context"
	"fmt"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// Mismatch is one table whose row counts disagree.
type Mismatch struct {
	Table  string
	Source int64
	Target int64
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%s: 源 %d 行，目标 %d 行", m.Table, m.Source, m.Target)
}

// Verify compares row counts table by table.
//
// A migration nobody can check is a migration nobody can trust, and the count
// is the check that costs nothing and catches the failures that actually
// happen — a table skipped, a batch that errored and was swallowed, a copy run
// against the wrong database.
//
// It is not a checksum. Comparing content would mean deciding what "equal"
// means for a timestamp that is text on one side and a timestamp on the other,
// and that decision belongs to the schema rather than to the verifier. The
// end-to-end tests read the copied rows back through the stores, which is the
// stronger check and the one that knows what the values mean.
//
// A target with more rows than the source is reported too: it means the copy
// ran into a database that was already being written to, which is a different
// mistake from a copy that lost rows and needs to be seen as one.
func Verify(ctx context.Context, src, dst *storage.DB) ([]Mismatch, error) {
	var out []Mismatch
	for _, table := range Tables {
		s, err := countRows(ctx, src, table)
		if err != nil {
			return nil, fmt.Errorf("count source %s: %w", table, err)
		}
		d, err := countRows(ctx, dst, table)
		if err != nil {
			return nil, fmt.Errorf("count target %s: %w", table, err)
		}
		if s != d {
			out = append(out, Mismatch{Table: table, Source: s, Target: d})
		}
	}
	return out, nil
}

// countRows answers zero for a table that is not there, because a SQLite
// deployment that never enabled a feature has no table for it and that is not
// a discrepancy.
func countRows(ctx context.Context, db *storage.DB, table string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n)
	if err != nil {
		if storage.IsMissingTable(err) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}
