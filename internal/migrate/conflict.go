package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/storage"
)

// maxConflictExamples caps what a ConflictError carries. Enough to see the
// shape of the problem; a page of them says nothing the first three didn't.
const maxConflictExamples = 5

// Conflict is one row the target already had, whose content differs from the
// source's.
type Conflict struct {
	Table  string
	Key    string // primary-key values, comma separated
	Column string // the column that differs; "" when the row is not there at all
	Source string
	Target string
}

// keyText renders a key for a person, which is a different job from keyOf's.
func keyText(row []any, keyIdx []int) string {
	parts := make([]string, len(keyIdx))
	for i, k := range keyIdx {
		parts[i] = canon(row[k])
	}
	return strings.Join(parts, ",")
}

func (c Conflict) String() string {
	if c.Column == "" {
		return fmt.Sprintf("%s[%s]: 目标库里根本没有这一行，插入却被某个唯一约束挡住了", c.Table, c.Key)
	}
	return fmt.Sprintf("%s[%s].%s: 源 %q，目标 %q", c.Table, c.Key, c.Column, c.Source, c.Target)
}

// ConflictError says the target already held rows with the same primary key
// but different content.
//
// Its own type, and fatal, because it is the one failure the row-count check
// cannot see: ON CONFLICT DO NOTHING keeps the row that is already there, so
// the counts come out equal and the migration reports success while the target
// says something different from the source. That is either the wrong target
// database or a source that changed under a resumed run, and both need a
// person.
type ConflictError struct {
	Table     string
	Conflicts []Conflict
	Total     int
}

func (e *ConflictError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: 目标库已有 %d 行主键相同但内容不同的数据 —— 没有覆盖它们。", e.Table, e.Total)
	b.WriteString("是不是搬错库了，或者服务已经在目标库上跑过、把它写变了？\n")
	for _, c := range e.Conflicts {
		b.WriteString("    " + c.String() + "\n")
	}
	if e.Total > len(e.Conflicts) {
		fmt.Fprintf(&b, "    …还有 %d 处\n", e.Total-len(e.Conflicts))
	}
	return strings.TrimRight(b.String(), "\n")
}

// keyOf renders one row's primary-key values as a string that stands for the
// row's identity.
//
// Length-prefixed rather than joined by a separator, because the result is
// used as a map key to match target rows against source rows and a separator
// is not injective: tool_results is keyed by five text columns, so ("a,b","c")
// and ("a","b,c") join to the same string. Two different rows colliding there
// means the copier compares one against the other's content and reports a
// conflict that is not one — or, the other way round, matches a row it should
// have flagged. No text can produce another key's encoding when each part
// carries its own length.
func keyOf(row []any, keyIdx []int) string {
	var b strings.Builder
	for _, k := range keyIdx {
		v := canon(row[k])
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(':')
		b.WriteString(v)
	}
	return b.String()
}

// compareExisting reads back the rows the insert did not add and reports the
// ones whose content differs.
//
// One query for the whole batch rather than one per row: the batch is already
// the unit the copier works in, and a resumed migration skips every row it
// already copied, so a per-row lookup would turn "run it again" into a walk.
func compareExisting(ctx context.Context, dst *storage.DB, table string, cols []string, rows [][]any, pk []string, keyIdx []int) ([]Conflict, int, error) {
	if len(rows) == 0 {
		return nil, 0, nil
	}
	where, args := keyFilter(pk, rows, keyIdx)
	q := `SELECT ` + strings.Join(cols, ",") + ` FROM ` + table + ` WHERE ` + where
	res, err := dst.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("read back %s: %w", table, err)
	}
	defer res.Close()

	have := map[string][]any{}
	for res.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := res.Scan(ptrs...); err != nil {
			return nil, 0, err
		}
		have[keyOf(vals, keyIdx)] = vals
	}
	if err := res.Err(); err != nil {
		return nil, 0, err
	}

	var out []Conflict
	total := 0
	for _, row := range rows {
		key := keyOf(row, keyIdx)
		target, ok := have[key]
		if !ok {
			// The key is not in the target, yet the insert did not add it:
			// something other than the primary key refused the row — a unique
			// index on different columns. Worth the same alarm; the row is
			// not there and the copy did not say so.
			total++
			if len(out) < maxConflictExamples {
				out = append(out, Conflict{Table: table, Key: keyText(row, keyIdx)})
			}
			continue
		}
		for i, c := range cols {
			s, d := canon(row[i]), canon(target[i])
			if s == d {
				continue
			}
			total++
			if len(out) < maxConflictExamples {
				out = append(out, Conflict{Table: table, Key: keyText(row, keyIdx), Column: c, Source: s, Target: d})
			}
			break // one column is enough to know this row differs
		}
	}
	return out, total, nil
}

// keyFilter builds the WHERE that selects exactly these rows by primary key.
func keyFilter(pk []string, rows [][]any, keyIdx []int) (string, []any) {
	args := make([]any, 0, len(rows)*len(keyIdx))
	var b strings.Builder
	if len(pk) == 1 {
		b.WriteString(pk[0] + " IN (" + storage.Placeholders(len(rows)) + ")")
		for _, row := range rows {
			args = append(args, row[keyIdx[0]])
		}
		return b.String(), args
	}
	// Row values rather than OR-of-ANDs: both dialects have had them for
	// years, and the OR form is what makes a planner give up on the index.
	b.WriteString("(" + strings.Join(pk, ",") + ") IN (")
	for i, row := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(" + storage.Placeholders(len(keyIdx)) + ")")
		for _, k := range keyIdx {
			args = append(args, row[k])
		}
	}
	b.WriteByte(')')
	return b.String(), args
}

// canon renders a value so the two dialects' spellings of the same thing come
// out equal.
//
// This is the part the row-count check was avoiding, and it is avoidable only
// as long as nothing compares content. The cases that matter:
//
//   - a timestamp SQLite keeps as RFC3339 text arrives back from a
//     PostgreSQL timestamptz as a time.Time, so both go to UTC RFC3339Nano;
//   - a text column reads as string on one driver and []byte on the other;
//   - a boolean is 0/1 on SQLite and true/false on PostgreSQL, which is the
//     same conversion coerce already does on the way in;
//   - a JSON document SQLite keeps as the exact bytes it was given comes back
//     from a PostgreSQL jsonb re-rendered — keys in a different order, a space
//     after every colon. ADK's events.content is one, so this is not a corner
//     case: without folding it, every migrated session looks like a conflict.
//
// Anything it renders differently for values that are really equal shows up as
// a loud, specific error naming the column — not as silent corruption — which
// is the direction a comparison like this should fail in.
func canon(v any) string {
	switch t := v.(type) {
	case nil:
		return "\x00NULL"
	case []byte:
		return canonString(string(t))
	case string:
		return canonString(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case bool:
		if t {
			return "1"
		}
		return "0"
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// canonString folds the two spellings of a timestamp, and of a JSON document,
// into one. Everything else is left alone.
func canonString(s string) string {
	if len(s) >= 20 && s[4] == '-' && s[7] == '-' {
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts.UTC().Format(time.RFC3339Nano)
		}
	}
	// Only an object or an array. A bare number or a quoted word is valid
	// JSON too, and folding those would silently equate the text "1" with the
	// number 1 in columns that are not JSON at all.
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		if j, ok := canonJSON(t); ok {
			return j
		}
	}
	return s
}

// canonJSON re-renders a document with its object keys sorted and no
// insignificant whitespace, which is what encoding/json does by default.
//
// UseNumber so a big integer id survives: decoding into float64 and encoding
// back would round it, and two ids that differ past 2^53 would compare equal.
func canonJSON(s string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	if dec.More() {
		return "", false // trailing content: not one document
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(out), true
}
