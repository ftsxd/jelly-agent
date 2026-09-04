<!--
  What the agent did, in order.

  A dumb component: it receives finished steps and renders them. Its props are
  `steps` and `dense` and nothing else — it is given no frames, so the folding
  rules stay in timeline.js where they can be tested without a DOM. That is a
  boundary worth keeping, because the previous arrangement grew the same
  assembly logic twice, once in each view, and the copies drifted.

  Tool arguments and results are rendered as text, never as HTML. They come
  from MCP servers and fetch_url, i.e. from whatever the agent happened to
  read, so putting them through v-html would reopen from a second door the hole
  markdown.js was written to close.
-->
<script setup>
import { computed, ref } from 'vue'
import Icon from './Icon.vue'
import { summarize, timelineSteps } from '../timeline'
import { evidenceId, fmtArgs, fmtTokens, isTruncated, prettyJSON, resultSummary, stepLabel } from '../format'

const props = defineProps({
  /** A timeline state from timeline.js. */
  timeline: { type: Object, required: true },
  /** Collapsed by default, for the transcript view. */
  dense: { type: Boolean, default: false },
})

const open = ref(new Set())
// Long runs are folded in the middle rather than virtualised: a few hundred
// rounds would otherwise all go into the DOM. The head and tail are what
// people look at; the middle is what they scroll past.
const FOLD_OVER = 50
const expandedAll = ref(false)

const steps = computed(() => timelineSteps(props.timeline))
const stats = computed(() => summarize(props.timeline))

const shown = computed(() => {
  const all = steps.value
  if (expandedAll.value || all.length <= FOLD_OVER) return all
  return [...all.slice(0, 20), { id: '__fold', kind: 'fold', hidden: all.length - 40 }, ...all.slice(-20)]
})

function toggle(id) {
  const next = new Set(open.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  open.value = next
}

function statusIcon(step) {
  if (step.status === 'failed') return 'alert'
  if (step.status === 'pending') return ''
  return 'check'
}
</script>

<template>
  <div v-if="steps.length" class="tl" :class="{ dense }">
    <div class="tl-head">
      <span class="tl-sum">
        {{ stats.steps }} 步
        <template v-if="stats.tools"> · {{ stats.tools }} 次工具</template>
        <template v-if="stats.agents > 1"> · {{ stats.agents }} 个 Agent</template>
        <template v-if="stats.rounds"> · {{ stats.rounds }} 轮</template>
        <span v-if="stats.failed" class="bad"> · {{ stats.failed }} 失败</span>
        <span v-if="stats.pending" class="wait"> · {{ stats.pending }} 执行中</span>
      </span>
      <span v-if="stats.usage.total" class="tl-tok mono">{{ fmtTokens(stats.usage.total) }} tok</span>
    </div>

    <ol class="tl-list">
      <li v-for="s in shown" :key="s.id" :style="{ '--depth': s.depth || 0 }" class="tl-row" :class="s.kind">
        <template v-if="s.kind === 'fold'">
          <button class="tl-fold" @click="expandedAll = true">
            展开中间省略的 {{ s.hidden }} 步
          </button>
        </template>

        <template v-else>
          <span class="tl-rail" aria-hidden="true" />
          <span class="tl-mark" :class="s.status || s.kind">
            <span v-if="s.status === 'pending'" class="spinner" />
            <Icon v-else-if="statusIcon(s)" :name="statusIcon(s)" :size="12" />
            <Icon v-else-if="s.kind === 'transfer'" name="link" :size="12" />
            <Icon v-else-if="s.kind === 'thought'" name="memory" :size="12" />
            <Icon v-else-if="s.kind === 'error'" name="alert" :size="12" />
            <Icon v-else name="chat" :size="12" />
          </span>

          <div class="tl-body">
            <button
              class="tl-line"
              type="button"
              :disabled="s.kind !== 'tool' && s.kind !== 'thought'"
              @click="toggle(s.id)"
            >
              <span class="tl-name mono">{{ stepLabel(s) }}</span>
              <span v-if="s.kind === 'tool' && s.args" class="tl-args mono">{{ fmtArgs(s.args) }}</span>
              <span v-if="s.agent && s.agent !== 'root'" class="tl-agent mono">{{ s.agent }}</span>
              <span
                v-if="s.approx"
                class="tl-flag"
                title="这一对调用与结果是按名称推测配对的，不是按调用 ID"
              >推测配对</span>
            </button>

            <div v-if="s.kind === 'tool'" class="tl-res" :class="s.status">{{ resultSummary(s) }}</div>
            <div v-else-if="s.kind === 'error'" class="tl-res failed">{{ s.text }}</div>
            <div v-else-if="s.kind === 'text'" class="tl-res">{{ s.text }}</div>
            <div v-else-if="s.kind === 'thought' && !open.has(s.id)" class="tl-res muted">
              {{ s.text }}
            </div>

            <div v-if="open.has(s.id)" class="tl-detail">
              <template v-if="s.kind === 'thought'">
                <pre class="mono">{{ s.text }}</pre>
              </template>
              <template v-else>
                <div v-if="evidenceId(s)" class="tl-ev mono">
                  证据 {{ evidenceId(s) }}
                  <span v-if="isTruncated(s)" class="tl-flag">已截断</span>
                </div>
                <div class="tl-kv">
                  <span class="tl-kv-k">参数</span>
                  <pre class="mono">{{ prettyJSON(s.args) || '（无）' }}</pre>
                </div>
                <div class="tl-kv">
                  <span class="tl-kv-k">返回</span>
                  <pre class="mono">{{ prettyJSON(s.response) || (s.error || '（无）') }}</pre>
                </div>
              </template>
            </div>
          </div>
        </template>
      </li>
    </ol>
  </div>
</template>

<style scoped>
.tl {
  border: 1px solid var(--hairline);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  overflow: hidden;
}
.tl-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  border-bottom: 1px solid var(--hairline);
  font-size: 12px;
  color: var(--text-dim);
}
.tl-sum .bad {
  color: var(--danger);
}
.tl-sum .wait {
  color: var(--text-muted);
}
.tl-tok {
  flex-shrink: 0;
  font-size: 11px;
  color: var(--text-muted);
}

.tl-list {
  margin: 0;
  padding: var(--sp-1) 0;
  list-style: none;
}
.tl-row {
  display: flex;
  align-items: flex-start;
  gap: var(--sp-2);
  padding: 3px var(--sp-3) 3px calc(var(--sp-3) + var(--depth) * 14px);
  position: relative;
}
.tl-row.failed {
  background: var(--danger-tint);
}

/* One hairline rail per nesting level, so a sub-agent's steps read as a
   branch rather than as a different indent. */
.tl-rail {
  position: absolute;
  left: calc(var(--sp-3) + var(--depth) * 14px - 7px);
  top: 0;
  bottom: 0;
  width: 1px;
  background: var(--hairline);
}
.tl-row:first-child .tl-rail {
  top: 50%;
}
.tl-row:last-child .tl-rail {
  bottom: 50%;
}

.tl-mark {
  flex-shrink: 0;
  width: 18px;
  height: 18px;
  display: grid;
  place-items: center;
  border-radius: 50%;
  background: var(--surface);
  box-shadow: 0 0 0 1px var(--hairline);
  color: var(--text-muted);
}
.tl-mark.ok {
  color: var(--accent);
}
.tl-mark.failed {
  color: var(--danger);
}
.tl-mark.transfer {
  color: var(--primary);
}

.tl-body {
  flex: 1;
  min-width: 0;
}
.tl-line {
  display: flex;
  align-items: baseline;
  gap: var(--sp-2);
  width: 100%;
  padding: 0;
  border: 0;
  background: none;
  text-align: left;
  font: inherit;
  color: var(--text);
  cursor: pointer;
}
.tl-line:disabled {
  cursor: default;
}
.tl-name {
  font-size: 12px;
  font-weight: 500;
  flex-shrink: 0;
}
.tl-args,
.tl-agent {
  font-size: 11px;
  color: var(--text-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tl-agent {
  flex-shrink: 0;
  margin-left: auto;
}
.tl-flag {
  flex-shrink: 0;
  padding: 0 4px;
  border-radius: 4px;
  background: var(--warning-tint);
  color: var(--warning);
  font-size: 10px;
}

.tl-res {
  font-size: 11px;
  color: var(--text-dim);
  line-height: 1.5;
  word-break: break-word;
}
.tl-res.pending {
  color: var(--text-muted);
}
.tl-res.failed {
  color: var(--danger);
}
.tl-res.muted {
  color: var(--text-muted);
  font-style: italic;
}

.tl-detail {
  margin-top: var(--sp-2);
  padding: var(--sp-2);
  border: 1px solid var(--hairline);
  border-radius: var(--radius-sm);
  background: var(--surface);
}
.tl-ev {
  margin-bottom: var(--sp-2);
  font-size: 11px;
  color: var(--text-muted);
}
.tl-kv + .tl-kv {
  margin-top: var(--sp-2);
}
.tl-kv-k {
  display: block;
  font-size: 10px;
  color: var(--text-muted);
  margin-bottom: 2px;
}
.tl-detail pre {
  margin: 0;
  font-size: 11px;
  line-height: 1.5;
  color: var(--text-dim);
  max-height: 260px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}

.tl-fold {
  width: 100%;
  padding: var(--sp-2);
  border: 0;
  border-radius: var(--radius-sm);
  background: var(--surface-3);
  color: var(--text-dim);
  font: inherit;
  font-size: 11px;
  cursor: pointer;
}

.tl.dense .tl-res,
.tl.dense .tl-args {
  display: -webkit-box;
  -webkit-line-clamp: 1;
  line-clamp: 1;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
</style>
