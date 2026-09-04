package tool

// Reading back a tool result that was shortened for the prompt.
//
// The prompt only ever sees a bounded copy of a large result. Everything the
// tool delivered is committed to durable storage first, under the same handle
// the model is shown, so the part that was cut is not lost — it just has to be
// asked for. This is that ask.
//
// Two focused tools rather than one with a mode parameter. What costs tokens
// is the full schema and description, not the count, and a union of parameters
// makes the model work out which ones apply to which mode — a reliable source
// of malformed calls. Reading and searching take different arguments and
// return different shapes, so they are different tools.
//
// Both are present on every turn, and that part is not a preference. Tools
// that appeared only when a truncated result existed would change the tool
// block from turn to turn, and the tool block is the head of the prompt-cache
// prefix: changing it forfeits the cache on the whole history behind it.
//
// There is no third tool for "how big is it". Every read carries the total
// size and line count, so the cheapest possible read answers that — one less
// schema in a prompt that pays for every one of them.

import (
	"context"
	"errors"
	"fmt"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/jelly-agent/jelly-agent/internal/record"
	"github.com/jelly-agent/jelly-agent/internal/telemetry"
)

// Names of the store's own readers. Exported so the keeper can exempt them
// from being stored: their output came out of the store already.
const (
	ReadResultName   = "read_result"
	SearchResultName = "search_result"
)

// ReadResultArgs is what the model supplies.
type ReadResultArgs struct {
	// Ref is the handle from a tool result's evidence_id, e.g. "e7".
	Ref string `json:"ref"`
	// Offset is where to start reading, in bytes. Zero starts at the
	// beginning; a follow-up read passes the previous answer's next_offset.
	Offset int `json:"offset,omitempty"`
	// Limit bounds one read. Zero takes the default window.
	Limit int `json:"limit,omitempty"`
}

// ReadResultOut is what comes back.
//
// NextOffset and HasMore are here so a model that needs more does not have to
// guess where it stopped. Leaving them out is how a caller ends up re-reading
// from zero, or deciding the rest is unreachable and giving up.
type ReadResultOut struct {
	Ref   string `json:"ref"`
	Tool  string `json:"tool"`
	Total int    `json:"total_bytes"`
	// Lines is the whole payload's line count, not this window's — so a first
	// cheap read tells the model the scale before it decides how to proceed.
	Lines  int    `json:"total_lines"`
	Offset int    `json:"offset"`
	Data   string `json:"data"`

	HasMore    bool `json:"has_more"`
	NextOffset int  `json:"next_offset,omitempty"`

	// UpstreamTruncated is what the tool itself said about shortening its own
	// output before this store ever saw it: "yes", "no", or "unknown". Unknown
	// is the honest answer for a third-party server that reports nothing, and
	// must not be read as "this is everything".
	UpstreamTruncated string `json:"upstream_truncated"`

	// Note tells the model what it can do next. Unlike the gateway's own
	// truncation — where a smaller page hits the same ceiling and retrying is
	// futile — reading further here genuinely works, and the difference has to
	// be said out loud or a model that learned the first lesson will not try.
	Note string `json:"note,omitempty"`
}

// Scoper resolves the conversation a call belongs to. The scope is the
// permission boundary, so it comes from the invocation rather than from
// anything the model can write: a handle must not be able to name another
// conversation's data.
type Scoper func(adktool.Context) record.Scope

// NewReadResultTool builds read_result over a delivery store.
func NewReadResultTool(store *record.Store, scope Scoper) (adktool.Tool, error) {
	if store == nil || scope == nil {
		return nil, errors.New("read_result: need a store and a scope")
	}
	return functiontool.New(
		functiontool.Config{
			Name: ReadResultName,
			Description: "按 evidence_id 分段读取某次工具调用已保存的完整返回。" +
				"当前可见信息不足，或需要精确搜索、计数时使用；信息已足够时直接回答。" +
				"用返回的 next_offset 继续读下一段。",
		},
		func(tc adktool.Context, args ReadResultArgs) (ReadResultOut, error) {
			return readResult(tc, store, scope(tc), args)
		},
	)
}

func readResult(ctx context.Context, store *record.Store, sc record.Scope, args ReadResultArgs) (ReadResultOut, error) {
	if args.Ref == "" {
		return ReadResultOut{}, fmt.Errorf("ref 不能为空，请传工具结果里的 evidence_id")
	}
	chunk, err := store.ReadLabel(ctx, sc, args.Ref, args.Offset, args.Limit)
	if errors.Is(err, record.ErrExpired) {
		// Deliberately different advice from not-found. The handle was real,
		// so looking for it again or trying a neighbouring one is wasted; the
		// only route to these bytes is running the tool again.
		return ReadResultOut{}, fmt.Errorf(
			"引用 %q 对应的结果已超过保留期，内容已清理——需要这些数据的话请重新调用产生它的工具，不要再尝试其他引用（%v）", args.Ref, err)
	}
	if errors.Is(err, record.ErrNotFound) {
		// Said plainly, because the alternative is a model that reads an empty
		// answer as "the tool found nothing" and reasons from it.
		return ReadResultOut{}, fmt.Errorf(
			"引用 %q 在本次会话中找不到对应的已保存结果——它可能来自其他会话，或该次调用的返回未能落库（结果里 retrievable 为假时即是如此）", args.Ref)
	}
	if err != nil {
		return ReadResultOut{}, err
	}

	next := chunk.Offset + len(chunk.Data)
	out := ReadResultOut{
		Ref: chunk.Label, Tool: chunk.Tool,
		Total: chunk.Total, Lines: chunk.Lines,
		Offset: chunk.Offset, Data: string(chunk.Data),
		HasMore:           next < chunk.Total,
		UpstreamTruncated: string(chunk.Upstream),
	}
	if out.HasMore {
		out.NextOffset = next
		out.Note = fmt.Sprintf("本段 %d 字节，共 %d 字节，还有更多。用 offset=%d 继续读取。",
			len(chunk.Data), chunk.Total, next)
	}
	if chunk.Upstream == record.UpstreamYes {
		out.Note += "注意：该工具在返回给系统之前已自行截断过输出，因此这里也不是它上游的完整内容。"
	}
	// Attributed to the tool that produced the bytes, not to read_result: the
	// question is which tools the model has to come back for.
	telemetry.RecordStoreBytes(ctx, telemetry.RecordServed, chunk.Tool, int64(len(chunk.Data)))
	return out, nil
}

// SearchResultArgs is what the model supplies to search a stored result.
type SearchResultArgs struct {
	// Ref is the handle from a tool result's evidence_id, e.g. "e7".
	Ref string `json:"ref"`
	// Pattern is a regular expression matched against each line.
	Pattern string `json:"pattern"`
	// Limit bounds how many matching lines come back. Zero takes the default.
	Limit int `json:"limit,omitempty"`
	// Context is how many surrounding lines to include on each side.
	Context int `json:"context,omitempty"`
}

// NewSearchResultTool builds search_result over a delivery store.
func NewSearchResultTool(store *record.Store, scope Scoper) (adktool.Tool, error) {
	if store == nil || scope == nil {
		return nil, errors.New("search_result: need a store and a scope")
	}
	return functiontool.New(
		functiontool.Config{
			Name: SearchResultName,
			Description: "在某次工具调用已保存的完整返回里按正则搜索（按行匹配，可带上下文）。" +
				"当前可见信息不足，或需要精确搜索、计数时使用；信息已足够时直接回答。" +
				"返回会给出命中总数，所以统计类问题应当搜索而不是逐段读完全文。",
		},
		func(tc adktool.Context, args SearchResultArgs) (record.SearchResult, error) {
			return searchResult(tc, store, scope(tc), args)
		},
	)
}

// searchResult is the body behind the tool, split out for the same reason
// readResult is: the closure above cannot be called without an ADK invocation
// context, and a search that is only tested through the store would not cover
// the argument handling or the not-found wording the model actually reads.
func searchResult(ctx context.Context, store *record.Store, sc record.Scope, args SearchResultArgs) (record.SearchResult, error) {
	if args.Ref == "" {
		return record.SearchResult{}, fmt.Errorf("ref 不能为空，请传工具结果里的 evidence_id")
	}
	res, err := store.Search(ctx, sc, args.Ref, args.Pattern,
		record.SearchOpts{Limit: args.Limit, Context: args.Context})
	if errors.Is(err, record.ErrExpired) {
		return record.SearchResult{}, fmt.Errorf(
			"引用 %q 对应的结果已超过保留期，内容已清理——需要这些数据的话请重新调用产生它的工具（%v）", args.Ref, err)
	}
	if errors.Is(err, record.ErrNotFound) {
		return record.SearchResult{}, fmt.Errorf(
			"引用 %q 在本次会话中找不到对应的已保存结果——它可能来自其他会话，或该次调用的返回未能落库（结果里 retrievable 为假时即是如此）", args.Ref)
	}
	if err == nil {
		// Only what came back, not the payload scanned: this counter measures
		// what the store put in front of the model, and search's whole point
		// is that those two numbers are very different.
		served := 0
		for _, h := range res.Hits {
			served += len(h.Text)
			for _, l := range h.Before {
				served += len(l)
			}
			for _, l := range h.After {
				served += len(l)
			}
		}
		telemetry.RecordStoreBytes(ctx, telemetry.RecordServed, res.Tool, int64(served))
	}
	return res, err
}
