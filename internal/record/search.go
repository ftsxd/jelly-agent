package record

// Searching inside a stored delivery.
//
// This is what turns the store from an archive into something the model can
// interrogate. Reading a large result page by page works but costs a page of
// tokens per page; searching costs one round trip and returns only the lines
// that matched.
//
// Two bounds, for different reasons. The number of hits returned is bounded
// because they go into the prompt. The number counted is bounded separately
// and much higher, because counting is cheap and an exact total is what lets
// the answer say "there are 4823 matches, narrow it" instead of leaving the
// model to guess whether it saw everything. When even counting hits its
// ceiling the total is reported as a floor, not as a fact.
//
// The pattern is a Go regexp, which is RE2: linear time, no backtracking, so a
// caller-supplied pattern cannot hang the process the way a PCRE one could.
// What it can still do is match an enormous number of positions, which is what
// the counting bound is for.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// Search bounds.
const (
	// MaxSearchBytes refuses to scan a payload larger than this. A delivery
	// that big is a different problem than search can solve.
	MaxSearchBytes = 64 << 20 // 64 MiB
	// MaxCountedHits bounds counting. Past it the total becomes a floor.
	MaxCountedHits = 100_000
	// DefaultHits and MaxHits bound what is returned, because that is what
	// costs prompt tokens.
	DefaultHits = 20
	MaxHits     = 200
	// maxLineRunes bounds one returned line. A minified JSON payload is one
	// enormous line, and returning it whole would defeat the point.
	maxLineRunes = 400
)

// Hit is one matching line with its surroundings.
type Hit struct {
	Line   int      `json:"line"` // 1-based
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// SearchResult is what a search found.
type SearchResult struct {
	Ref   string `json:"ref"`
	Tool  string `json:"tool"`
	Lines int    `json:"lines"`
	Bytes int    `json:"bytes"`

	// Total is how many lines matched. Exact says whether that number is the
	// count or a floor — the distinction matters, because "4823 matches, show
	// fewer" and "at least 100000 matches, this pattern is too broad" call for
	// different next moves.
	Total int  `json:"total_matches"`
	Exact bool `json:"total_exact"`

	Hits []Hit `json:"hits"`
	// Truncated says hits were left out, not that matching stopped.
	Truncated bool `json:"truncated"`
}

// SearchOpts bounds one search.
type SearchOpts struct {
	Limit   int // hits returned; zero takes the default
	Context int // lines of context on each side
}

// Search runs a pattern over one delivery, addressed by its handle.
func (s *Store) Search(ctx context.Context, sc Scope, label, pattern string, opts SearchOpts) (SearchResult, error) {
	if s == nil || s.db == nil {
		return SearchResult{}, errors.New("record: store not open")
	}
	if strings.TrimSpace(pattern) == "" {
		return SearchResult{}, errors.New("record: 搜索模式不能为空")
	}
	seq, ok := parseLabel(label)
	if !ok {
		return SearchResult{}, ErrNotFound
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		// Handed back as-is: the model wrote the pattern and can fix it, but
		// only if it is told what was wrong with it.
		return SearchResult{}, fmt.Errorf("record: 无法编译搜索模式 %q: %w", pattern, err)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultHits
	}
	if limit > MaxHits {
		limit = MaxHits
	}
	before := max(0, min(opts.Context, 20))

	var (
		res SearchResult
		buf []byte
	)
	var expired string
	err = s.db.QueryRowContext(ctx, `
		SELECT tool, bytes, expired_at, payload FROM tool_results
		WHERE app_name=? AND user_id=? AND session_id=? AND seq=?`,
		sc.AppName, sc.UserID, sc.SessionID, seq,
	).Scan(&res.Tool, &res.Bytes, &expired, &buf)
	if errors.Is(err, sql.ErrNoRows) {
		return SearchResult{}, ErrNotFound
	}
	if err != nil {
		return SearchResult{}, fmt.Errorf("record: search %s/%s: %w", sc.SessionID, label, err)
	}
	if expired != "" {
		// Same distinction the read path draws: a search over a dropped
		// payload would report zero matches, which the model would take as
		// evidence rather than as an absence of it.
		return SearchResult{}, fmt.Errorf("%w: %s 于 %s 过期（原本 %d 字节）",
			ErrExpired, Label(seq), expired, res.Bytes)
	}
	res.Ref = Label(seq)
	// The same view the read path uses, so a hit's line number and the totals
	// both describe the bytes the caller will read back.
	buf = ops.TextView(buf)
	res.Bytes = len(buf)
	if len(buf) > MaxSearchBytes {
		return SearchResult{}, fmt.Errorf(
			"record: 结果 %s 有 %d 字节，超出可搜索上限 %d，请改用分段读取", label, len(buf), MaxSearchBytes)
	}

	scanned, err := scanMatches(ctx, bytes.NewReader(buf), re, limit, before, MaxCountedHits, len(buf))
	if err != nil {
		return SearchResult{}, err
	}
	res.Total, res.Exact, res.Hits, res.Truncated, res.Lines =
		scanned.Total, scanned.Exact, scanned.Hits, scanned.Truncated, scanned.Lines
	return res, nil
}

// scanMatches walks a reader line by line and collects matches.
//
// Split out from Search so that cancellation part-way through a long scan can
// be tested from a reader that cancels, rather than from a timing race. The
// check matters: the payload ceiling bounds the input but a large pattern over
// sixty megabytes is still seconds of work, and the invocation's context is
// what ends it when the caller has gone away.
//
// countCap is a parameter rather than a reference to MaxCountedHits so that the
// floor path — the one that reports "at least N" instead of a count — can be
// reached by a test without writing a hundred thousand matching lines. An
// untested contract about honesty is not a contract.
func scanMatches(ctx context.Context, r io.Reader, re *regexp.Regexp, limit, before, countCap, size int) (SearchResult, error) {
	var res SearchResult
	res.Exact = true
	ring := make([]string, 0, before)
	// Indices, not pointers into res.Hits: appending to that slice can move
	// its backing array, and a pointer taken before the move writes trailing
	// context into memory nothing reads.
	pending := make([]int, 0, before)

	sc2 := bufio.NewScanner(r)
	// A minified payload is one line as long as the whole result, so the
	// scanner needs room for it rather than failing with "token too long".
	sc2.Buffer(make([]byte, 0, 64<<10), size+1)
	line := 0
	for sc2.Scan() {
		if err := ctx.Err(); err != nil {
			return SearchResult{}, err
		}
		line++
		text := clip(sc2.Text())

		// Fill the "after" context of hits still waiting for it.
		for i := 0; i < len(pending); {
			h := &res.Hits[pending[i]]
			if len(h.After) < before {
				h.After = append(h.After, text)
			}
			if len(h.After) >= before {
				pending = append(pending[:i], pending[i+1:]...)
				continue
			}
			i++
		}

		if re.MatchString(sc2.Text()) {
			if res.Total < countCap {
				res.Total++
			} else {
				res.Exact = false
			}
			if len(res.Hits) < limit {
				res.Hits = append(res.Hits, Hit{
					Line: line, Text: text,
					Before: append([]string(nil), ring...),
				})
				if before > 0 {
					pending = append(pending, len(res.Hits)-1)
				}
			} else {
				res.Truncated = true
			}
		}

		if before > 0 {
			if len(ring) == before {
				ring = ring[1:]
			}
			ring = append(ring, text)
		}
	}
	if err := sc2.Err(); err != nil {
		return SearchResult{}, fmt.Errorf("record: scan: %w", err)
	}
	res.Lines = line
	return res, nil
}

// clip bounds one line by runes, so a minified payload's single enormous line
// does not arrive whole.
func clip(s string) string {
	r := []rune(s)
	if len(r) <= maxLineRunes {
		return s
	}
	return string(r[:maxLineRunes]) + "…"
}
