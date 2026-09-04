package tool

// Reading back a tool result that was shortened for the prompt.
//
// The prompt only ever sees a bounded copy of a large result. Everything the
// tool delivered is committed to durable storage first, under the same handle
// the model is shown, so the part that was cut is not lost — it just has to be
// asked for. This is that ask.
//
// Deliberately one tool rather than several. Not because one is inherently
// right — a couple of focused tools can be smaller than one with a union of
// parameters, and what costs tokens is the full schema and description, not
// the count — but because these must be present on every turn. Tools that
// appear only when a truncated result exists would change the tool block from
// turn to turn, and the tool block is the head of the prompt-cache prefix:
// changing it forfeits the cache on the whole history behind it. A small fixed
// schema is the shape that does not have to choose between availability and
// prefix stability.

import (
	"context"
	"errors"
	"fmt"

	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/jelly-agent/jelly-agent/internal/record"
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
	Ref    string `json:"ref"`
	Tool   string `json:"tool"`
	Total  int    `json:"total_bytes"`
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
			Name: "read_result",
			Description: "按 evidence_id 读取某次工具调用的完整返回（分段）。" +
				"当结果里 truncated 为真、或需要被省略的细节时使用；" +
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
		Total: chunk.Total, Offset: chunk.Offset, Data: string(chunk.Data),
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
	return out, nil
}
