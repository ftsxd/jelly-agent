package toolreg

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/ops"
	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// DBSource is the console's own layer, kept in the database rather than in a
// file it rewrites.
//
// It replaces console.yaml, and the reason is an incident: on 2026-09-08 a
// save from the console wrote the whole file back with one field cleared, and
// there was no history to find out what had been there. A file rewritten in
// full on every save also loses updates between processes — the lock that
// guarded it was a mutex in one of them.
//
// A row per tool with nullable columns says the same thing structurally: NULL
// means "this layer declares nothing here, keep what the base said", which is
// what the patch semantics needed and what a YAML zero value could never
// express.
type DBSource struct {
	db   *storage.DB
	poll time.Duration
}

// NewDBSource reads the console's declarations from db.
func NewDBSource(db *storage.DB) *DBSource {
	return &DBSource{db: db, poll: declPollInterval}
}

// WithPoll sets how often Watch asks. Returns s, so it reads as one
// expression at the call site.
//
// For tests, which would otherwise have to wait out the production interval
// to find out whether anything is wired to Watch at all — and a test that
// takes three seconds to answer that is a test people stop running.
func (s *DBSource) WithPoll(d time.Duration) *DBSource {
	if d > 0 {
		s.poll = d
	}
	return s
}

// declPollInterval is how often Watch asks whether anything changed.
//
// It asks for one integer — the highest id in the change log — so the cost of
// asking is a single indexed read, and the interval can be short enough that
// a second process notices an edit while the person who made it is still
// looking at the page.
const declPollInterval = 3 * time.Second

func (s *DBSource) Name() string { return "db:tool_decls" }

// IsOverlay marks this as a patch over the file sources. See Overlay.
func (*DBSource) IsOverlay() {}

// Load reads every declaration.
//
// A NULL column is left as the zero value, which applyOverlay reads as "not
// declared" — so the two representations line up without a translation table.
func (s *DBSource) Load(ctx context.Context) ([]ops.ToolMetadata, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT server, name, description, use_cases, examples, anti_examples,
		       suites, produces, side_effect
		FROM tool_decls ORDER BY server, name`)
	if err != nil {
		if storage.IsMissingTable(err) {
			// A deployment that has not created the table yet declares
			// nothing, which is a state and not a failure.
			return nil, nil
		}
		return nil, fmt.Errorf("toolreg: read tool_decls: %w", err)
	}
	defer rows.Close()

	var out []ops.ToolMetadata
	for rows.Next() {
		var (
			m                                        ops.ToolMetadata
			desc, produces, effect                   *string
			useCases, examples, antiExamples, suites *string
		)
		if err := rows.Scan(&m.Server, &m.Name, &desc, &useCases, &examples,
			&antiExamples, &suites, &produces, &effect); err != nil {
			return nil, fmt.Errorf("toolreg: scan tool_decls: %w", err)
		}
		if desc != nil {
			m.Description = *desc
		}
		if produces != nil {
			m.Produces = ops.EvidenceKind(*produces)
		}
		if effect != nil {
			m.SideEffect = ops.SideEffectLevel(*effect)
		}
		for _, f := range []struct {
			raw *string
			dst *[]string
		}{
			{useCases, &m.UseCases}, {examples, &m.Examples},
			{antiExamples, &m.AntiExamples}, {suites, &m.Suites},
		} {
			if f.raw == nil {
				continue
			}
			if err := json.Unmarshal([]byte(*f.raw), f.dst); err != nil {
				return nil, fmt.Errorf("toolreg: %s/%s 的列存了不是 JSON 数组的东西: %w",
					m.Server, m.Name, err)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Watch yields a fresh set whenever the change log grows.
//
// Polling one integer rather than comparing file modification times, which is
// what the file source did and which no second process could see. The log's
// highest id is a version number that every process reads the same way, so an
// edit made in one console tab reaches the other one's registry.
func (s *DBSource) Watch(ctx context.Context) <-chan []ops.ToolMetadata {
	ch := make(chan []ops.ToolMetadata, 1)
	go func() {
		defer close(ch)
		t := time.NewTicker(s.poll)
		defer t.Stop()
		seen := s.version(ctx) // baseline, so the first tick is not a false positive
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			v := s.version(ctx)
			if v == seen {
				continue
			}
			seen = v
			metas, err := s.Load(ctx)
			if err != nil {
				continue // reported by the next successful load; a poll is not a request
			}
			select {
			case ch <- metas:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// version is the highest id in the change log, or zero.
func (s *DBSource) version(ctx context.Context) int64 {
	var v *int64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM tool_decl_log`).Scan(&v); err != nil {
		return 0
	}
	if v == nil {
		return 0
	}
	return *v
}
