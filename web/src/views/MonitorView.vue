<script setup>
import { computed, onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const stats = ref(null)
const loading = ref(true)
const error = ref('')

// The fixed prompt: what goes out on every model call regardless of what the
// user asked. It is the part a growing bill usually comes from, and the one
// the provider's single input-token figure cannot separate out.
const prompt = ref(null)
const promptOpen = ref(false)
const openPart = ref('')

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [s, p] = await Promise.all([api.stats(), api.prompt().catch(() => null)])
    stats.value = s
    prompt.value = p
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

const promptParts = computed(() => (prompt.value?.parts || []).filter((p) => !p.assembled))
const assembled = computed(() => (prompt.value?.parts || []).find((p) => p.assembled) || null)
// Truncating a JSON result mid-structure leaves the model unable to use it,
// and a model that cannot use a result asks again — which is why this bound is
// worth seeing next to the tool list rather than buried in a YAML file.
const boundedTools = computed(() => (prompt.value?.tools || []).filter((t) => t.max_result_bytes))

// Human-readable counts: 12.3k / 4.5M, plain ints below 1000.
function fmt(n) {
  if (n == null) return '0'
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M'
  if (n >= 1_000) return (n / 1_000).toFixed(1).replace(/\.0$/, '') + 'k'
  return String(n)
}

const kpis = computed(() => {
  const s = stats.value
  if (!s) return []
  return [
    { label: '会话', value: fmt(s.sessions), icon: 'sessions' },
    { label: '消息轮次', value: fmt(s.messages), icon: 'chat' },
    { label: '工具调用', value: fmt(s.tool_calls), icon: 'tool' },
    { label: 'Token 总量', value: fmt(s.tokens?.total), icon: 'spark' },
  ]
})

const topToolCount = computed(() =>
  Math.max(1, ...(stats.value?.tools ?? []).map((t) => t.count)),
)

const maxDailyTokens = computed(() =>
  Math.max(1, ...(stats.value?.daily ?? []).map((d) => d.tokens)),
)

// Short MM-DD label for the daily axis.
function dayLabel(date) {
  return date.slice(5)
}

// Every tool the ranking above shows is listed here too, recorded or not.
//
// Filtering out the un-recorded ones was the first attempt and it read as a
// bug: a tool visible in one panel and absent from the next looks like data
// went missing, when the truth is only that timing did not exist yet. Showing
// the row with dashes says that directly.
const timedTools = computed(() => stats.value?.tools ?? [])

const anyTimed = computed(() => timedTools.value.some((t) => t.timed > 0))

const telemetrySince = computed(() => {
  const raw = stats.value?.telemetry?.since
  if (!raw) return ''
  const d = new Date(raw)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString()
})

function ms(v) {
  if (v == null) return '—'
  return v >= 1000 ? (v / 1000).toFixed(1) + 's' : v + 'ms'
}

function okRate(t) {
  if (!t.timed) return '—'
  return Math.round((100 * (t.ok ?? 0)) / t.timed) + '%'
}

// A slow call is worth flagging even when it succeeds: the time is spent either
// way, and the p95 is where a five-minute budget actually goes.
function slow(v) {
  return v >= 5000
}

// Failure causes, most frequent first. Two buckets that both read as "it broke"
// lead to completely different fixes — a timeout points at the network or an
// over-long deadline, an upstream error points at the other system.
const errKindLabels = {
  bad_args: '参数错误',
  timeout: '超时',
  canceled: '被取消',
  auth: '鉴权失败',
  not_found: '目标不存在',
  upstream: '下游故障',
  unknown: '未归类',
}

function errKinds(t) {
  return Object.entries(t.err_kinds ?? {})
    .map(([kind, count]) => ({ kind, count, label: errKindLabels[kind] ?? kind }))
    .sort((a, b) => b.count - a.count)
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <h1>监控</h1>
      <button class="btn" @click="load" :disabled="loading">
        <Icon name="refresh" :size="16" /> 刷新
      </button>
    </header>

    <div class="body">
      <div v-if="loading" class="empty"><span class="spinner" /> 加载中…</div>
      <div v-else-if="error" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>

      <template v-else-if="stats">
        <section class="kpis">
          <div v-for="k in kpis" :key="k.label" class="card kpi">
            <Icon :name="k.icon" :size="18" class="kpi-icon" />
            <div class="kpi-num">{{ k.value }}</div>
            <div class="kpi-label">{{ k.label }}</div>
          </div>
        </section>

        <section class="grid">
          <div class="card panel">
            <h2 class="section-title">Token 构成</h2>
            <div class="token-rows">
              <div class="token-row">
                <span class="token-name">输入 (prompt)</span>
                <span class="mono token-val">{{ fmt(stats.tokens.prompt) }}</span>
              </div>
              <div class="token-row">
                <span class="token-name">输出 (completion)</span>
                <span class="mono token-val">{{ fmt(stats.tokens.completion) }}</span>
              </div>
              <div class="token-row total">
                <span class="token-name">合计</span>
                <span class="mono token-val">{{ fmt(stats.tokens.total) }}</span>
              </div>
            </div>

            <div class="meta-line">
              <div class="meta-item">
                <span class="dim">默认 Provider</span>
                <span class="mono">{{ stats.providers.default || '—' }}</span>
              </div>
              <div class="meta-item">
                <span class="dim">已配置 Provider</span>
                <span class="mono">{{ stats.providers.count }}</span>
              </div>
              <div class="meta-item">
                <span class="dim">会话检索 (L2)</span>
                <span class="badge" :class="stats.memory.search_enabled ? 'on' : 'off'">
                  {{ stats.memory.search_enabled ? '已启用' : '未启用' }}
                </span>
              </div>
            </div>
          </div>

          <div class="card panel">
            <h2 class="section-title">工具调用排行</h2>
            <div v-if="stats.tools.length" class="bars">
              <div v-for="t in stats.tools" :key="t.name" class="bar-row">
                <span class="mono bar-name" :title="t.name">{{ t.name }}</span>
                <div class="bar-track">
                  <div class="bar-fill" :style="{ width: (t.count / topToolCount) * 100 + '%' }" />
                </div>
                <span class="mono bar-val">{{ t.count }}</span>
              </div>
            </div>
            <div v-else class="empty sm">暂无工具调用记录</div>
          </div>
        </section>

        <section class="card panel">
          <div class="panel-head">
            <h2 class="section-title">工具耗时与失败归因</h2>
            <span v-if="telemetrySince" class="dim sm">埋点自 {{ telemetrySince }} 起</span>
          </div>
          <div v-if="anyTimed" class="table-wrap">
            <table class="tl">
              <thead>
                <tr>
                  <th class="tl-name">工具</th>
                  <th class="num">已记录 / 总次数</th>
                  <th class="num">成功率</th>
                  <th class="num">p50</th>
                  <th class="num">p95</th>
                  <th class="num">最慢</th>
                  <th>失败归因</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="t in timedTools" :key="t.name">
                  <td class="mono tl-name">{{ t.name }}</td>
                  <td class="num mono">
                    {{ t.timed }}<span class="dim"> / {{ t.count }}</span>
                  </td>
                  <td class="num mono" :class="{ bad: t.timed > 0 && t.ok < t.timed }">
                    {{ okRate(t) }}
                  </td>
                  <td class="num mono">{{ t.timed ? ms(t.p50_ms) : '—' }}</td>
                  <td class="num mono" :class="{ bad: t.timed > 0 && slow(t.p95_ms) }">
                    {{ t.timed ? ms(t.p95_ms) : '—' }}
                  </td>
                  <td class="num mono dim">{{ t.timed ? ms(t.max_ms) : '—' }}</td>
                  <td>
                    <span v-if="!errKinds(t).length" class="dim">—</span>
                    <span v-for="e in errKinds(t)" :key="e.kind" class="kind">
                      {{ e.label }} <b>{{ e.count }}</b>
                    </span>
                  </td>
                </tr>
              </tbody>
            </table>
            <p class="note sm dim">
              「已记录」是埋点启用之后的调用数，「总次数」来自全部历史会话。两者不等是正常的：单次耗时从来没写进会话事件，早先的调用无法补算，所以那些行只有次数、没有耗时。
            </p>
          </div>
          <div v-else class="empty sm">尚无耗时记录。跑一次带工具调用的对话后即可看到。</div>
        </section>

        <section class="card panel">
          <h2 class="section-title">每日 Token 趋势</h2>
          <div v-if="stats.daily.length" class="chart">
            <div v-for="d in stats.daily" :key="d.date" class="chart-col" :title="`${d.date} · ${d.tokens} tokens · ${d.messages} 条消息`">
              <div class="chart-bar-wrap">
                <div class="chart-bar" :style="{ height: (d.tokens / maxDailyTokens) * 100 + '%' }" />
              </div>
              <span class="chart-label mono dim">{{ dayLabel(d.date) }}</span>
            </div>
          </div>
          <div v-else class="empty sm">暂无数据</div>
        </section>

        <section v-if="prompt" class="card">
          <h2 class="section-title">
            提示词构成
            <button class="btn btn-sm" @click="promptOpen = !promptOpen">
              {{ promptOpen ? '收起' : '展开' }}
            </button>
          </h2>
          <p class="muted note">
            每次模型调用都会重发这部分，与用户问什么无关。它不出现在 Provider 报的单个
            输入 token 数里，所以一次运行贵得莫名时，通常先看这里。
          </p>
          <div class="pm-totals">
            <span>系统指令 <b class="mono">{{ fmt(prompt.totals.system_tokens) }}</b></span>
            <span>工具描述 <b class="mono">{{ fmt(prompt.totals.tools_tokens) }}</b></span>
            <span>固定合计 <b class="mono">{{ fmt(prompt.totals.fixed_tokens) }}</b></span>
            <span>工具数 <b class="mono">{{ prompt.totals.tools }}</b><template v-if="prompt.totals.max_tools"> / 上限 {{ prompt.totals.max_tools }}</template></span>
          </div>

          <template v-if="promptOpen">
            <div class="pm-list">
              <div v-for="p in promptParts" :key="p.name" class="pm-part">
                <button class="pm-head" @click="openPart = openPart === p.name ? '' : p.name">
                  <span class="pm-name">{{ p.name }}</span>
                  <span class="mono dim">{{ fmt(p.tokens) }} tok</span>
                </button>
                <pre v-if="openPart === p.name" class="mono pm-text">{{ p.text }}</pre>
              </div>
              <div v-if="assembled" class="pm-part">
                <button class="pm-head" @click="openPart = openPart === '__all' ? '' : '__all'">
                  <span class="pm-name">完整拼装（发给模型的原文）</span>
                  <span class="mono dim">{{ fmt(assembled.tokens) }} tok</span>
                </button>
                <pre v-if="openPart === '__all'" class="mono pm-text">{{ assembled.text }}</pre>
              </div>
            </div>

            <h3 class="pm-sub">工具（每轮都随请求发出）</h3>
            <div class="pm-tools">
              <div v-for="t in prompt.tools" :key="t.name" class="pm-tool">
                <span class="mono pm-tool-name">{{ t.name }}</span>
                <span v-if="t.server" class="badge">{{ t.server }}</span>
                <span class="badge" :class="t.side_effect">{{ t.side_effect }}</span>
                <span v-if="t.max_result_bytes" class="mono dim pm-bound">
                  返回上限 {{ t.max_result_bytes }}B
                </span>
                <span class="mono dim">{{ fmt(t.tokens) }} tok</span>
              </div>
            </div>
            <p v-if="boundedTools.length" class="muted note">
              返回上限会在超出时截断结果。JSON 被从中间切断时模型无法使用，往往会换个参数
              重试——每次重试都要把整段历史重发一遍，代价远超省下的字节。
            </p>
          </template>
        </section>
      </template>
    </div>
  </div>
</template>

<style scoped>
.view {
  display: flex;
  flex-direction: column;
  height: 100%;
}
.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--sp-4) var(--sp-5);
  border-bottom: 1px solid var(--border);
}
.topbar h1 {
  font-size: 18px;
}
.body {
  flex: 1;
  overflow-y: auto;
  padding: var(--sp-5);
  display: flex;
  flex-direction: column;
  gap: var(--sp-5);
}
.section-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  margin-bottom: var(--sp-4);
}

.kpis {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: var(--sp-4);
}
.kpi {
  padding: var(--sp-4);
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
  transition: transform 0.18s ease, border-color 0.18s ease, box-shadow 0.18s ease;
}
.kpi:hover {
  transform: translateY(-1px);
  border-color: var(--border-strong);
  box-shadow: 0 2px 8px rgba(15, 23, 42, 0.06);
}
.kpi-icon {
  box-sizing: content-box;
  width: 18px;
  height: 18px;
  padding: 8px;
  border-radius: var(--radius-sm);
  background: var(--primary-tint);
  color: var(--primary);
}
/* Per-KPI hue coding: blue / violet / teal / amber, carried by the border and
   the icon chip only. The dark theme washed the whole card in its hue; four
   tinted cards in a row on a light ground read as a warning panel rather than
   as a set of numbers. */
.kpi:nth-child(1) {
  border-color: color-mix(in srgb, var(--primary) 26%, transparent);
}
.kpi:nth-child(1):hover {
  border-color: color-mix(in srgb, var(--primary) 50%, transparent);
}
.kpi:nth-child(2) {
  border-color: color-mix(in srgb, var(--primary-2) 26%, transparent);
}
.kpi:nth-child(2):hover {
  border-color: color-mix(in srgb, var(--primary-2) 50%, transparent);
}
.kpi:nth-child(3) {
  border-color: color-mix(in srgb, var(--accent) 26%, transparent);
}
.kpi:nth-child(3):hover {
  border-color: color-mix(in srgb, var(--accent) 50%, transparent);
}
.kpi:nth-child(4) {
  border-color: rgba(240, 180, 84, 0.26);
}
.kpi:nth-child(4):hover {
  border-color: rgba(240, 180, 84, 0.5);
}
.kpi:nth-child(1) .kpi-icon {
  background: color-mix(in srgb, var(--primary) 20%, transparent);
  color: var(--primary-hover);
}
.kpi:nth-child(2) .kpi-icon {
  background: color-mix(in srgb, var(--primary-2) 20%, transparent);
  color: var(--primary-2);
}
.kpi:nth-child(3) .kpi-icon {
  background: color-mix(in srgb, var(--accent) 18%, transparent);
  color: var(--accent);
}
.kpi:nth-child(4) .kpi-icon {
  background: rgba(240, 180, 84, 0.18);
  color: var(--warning);
}
.kpi-num {
  font-size: 28px;
  font-weight: 700;
  letter-spacing: -0.02em;
  margin-top: var(--sp-2);
}
.kpi-label {
  font-size: 12px;
  color: var(--text-dim);
}

.grid {
  display: grid;
  grid-template-columns: minmax(280px, 360px) 1fr;
  gap: var(--sp-5);
  align-items: start;
}
.panel {
  padding: var(--sp-4) var(--sp-5);
}

.token-rows {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.token-row {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  padding: var(--sp-2) 0;
  font-size: 14px;
}
.token-row.total {
  border-top: 1px solid var(--border);
  margin-top: var(--sp-1);
  font-weight: 600;
}
.token-name {
  color: var(--text-dim);
}
.token-val {
  font-size: 15px;
}

.meta-line {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-4);
  margin-top: var(--sp-4);
  padding-top: var(--sp-4);
  border-top: 1px solid var(--border);
}
.meta-item {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: 13px;
}
.meta-item .dim {
  font-size: 11px;
}
.badge {
  font-size: 12px;
  padding: 1px 8px;
  border-radius: 999px;
  width: fit-content;
}
.badge.on {
  background: var(--accent-tint);
  color: var(--accent);
}
.badge.off {
  background: var(--surface-3);
  color: var(--text-muted);
}

.bars {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.bar-row {
  display: grid;
  grid-template-columns: 130px 1fr 44px;
  align-items: center;
  gap: var(--sp-3);
  font-size: 13px;
}
.bar-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text-dim);
}
.bar-track {
  height: 8px;
  background: var(--surface-3);
  border-radius: 999px;
  overflow: hidden;
}
.bar-fill {
  height: 100%;
  /* The nth-child rules below give each bar its own hue; this is the base for
     any beyond the fifth. */
  background: var(--primary);
  border-radius: 999px;
  min-width: 2px;
  transition: width 0.3s ease;
}
/* Tool ranking: categorical palette cycling blue/violet/teal/amber/cyan.
   Flat fills rather than the dark theme's gradient-plus-glow — a glow on a
   white ground reads as a printing error, and the bar's own length is what
   carries the value. */
.bar-row:nth-child(5n + 1) .bar-fill {
  background: var(--primary);
}
.bar-row:nth-child(5n + 2) .bar-fill {
  background: var(--primary-2);
}
.bar-row:nth-child(5n + 3) .bar-fill {
  background: var(--accent);
}
.bar-row:nth-child(5n + 4) .bar-fill {
  background: var(--warning);
}
.bar-row:nth-child(5n + 5) .bar-fill {
  background: var(--cyan);
}
.panel-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.table-wrap {
  overflow-x: auto;
}

.tl {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}

.tl th,
.tl td {
  padding: 8px 10px;
  text-align: left;
  border-bottom: 1px solid var(--border);
  white-space: nowrap;
}

.tl th {
  font-size: 11px;
  font-weight: 500;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  opacity: 0.6;
}

.tl tbody tr:last-child td {
  border-bottom: none;
}

.tl .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
}

/* The name is the only column that makes the rest of the row mean anything, so
   it stays put when the table scrolls sideways. Its background has to match the
   panel it sits on — a fallback colour here paints a light slab over a dark
   card and hides the very labels the column exists to keep visible. */
.tl-name {
  position: sticky;
  left: 0;
  background: var(--surface);
  z-index: 1;
}

.tl td.bad {
  color: var(--danger);
}

.kind {
  display: inline-block;
  margin-right: 6px;
  padding: 1px 7px;
  border-radius: 999px;
  background: var(--danger-tint);
  border: 1px solid var(--danger-border);
  color: var(--danger);
  font-size: 12px;
}

.note {
  margin: 10px 0 0;
  line-height: 1.6;
  white-space: normal;
}

.bar-val {
  text-align: right;
  color: var(--text);
}

.chart {
  display: flex;
  align-items: flex-end;
  gap: var(--sp-2);
  height: 180px;
  overflow-x: auto;
  padding-bottom: var(--sp-1);
}
.chart-col {
  flex: 1;
  min-width: 28px;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--sp-2);
  height: 100%;
}
.chart-bar-wrap {
  flex: 1;
  width: 100%;
  display: flex;
  align-items: flex-end;
  justify-content: center;
}
.chart-bar {
  width: 60%;
  max-width: 32px;
  min-height: 2px;
  /* Flat. A vertical gradient on a bar makes the top read as a different
     value from the bottom — the bar's height already carries the number, and
     on a light ground the fade at the base looked like the bar was cut off. */
  background: var(--primary);
  border-radius: var(--radius-sm) var(--radius-sm) 0 0;
  transition: height 0.3s ease, filter 0.15s ease, box-shadow 0.15s ease;
}
.chart-col:hover .chart-bar {
  filter: brightness(1.25);
}
.chart-label {
  font-size: 10px;
  white-space: nowrap;
}

.empty.sm {
  min-height: 0;
  padding: var(--sp-4);
  font-size: 13px;
}
.error-bar {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  background: var(--danger-tint);
  color: var(--danger);
  border-radius: var(--radius-sm);
  font-size: 13px;
}

@media (max-width: 900px) {
  .kpis {
    grid-template-columns: repeat(2, 1fr);
  }
  .grid {
    grid-template-columns: 1fr;
  }
}
/* Prompt composition */
.section-title .btn-sm {
  margin-left: auto;
  font-size: 11px;
  padding: 1px 8px;
}
.note {
  margin: 0 0 var(--sp-3);
  font-size: 12px;
  line-height: 1.6;
}
.pm-totals {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-2) var(--sp-4);
  padding: var(--sp-3);
  border: 1px solid var(--hairline);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  font-size: 12px;
  color: var(--text-dim);
}
.pm-totals b {
  color: var(--text);
}
.pm-list {
  margin-top: var(--sp-3);
  display: grid;
  gap: var(--sp-1);
}
.pm-part {
  border: 1px solid var(--hairline);
  border-radius: var(--radius-sm);
  overflow: hidden;
}
.pm-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
  width: 100%;
  padding: var(--sp-2) var(--sp-3);
  border: 0;
  background: var(--surface);
  font: inherit;
  font-size: 12px;
  color: var(--text);
  cursor: pointer;
  text-align: left;
}
.pm-head:hover {
  background: var(--surface-3);
}
.pm-name {
  font-weight: 500;
}
.pm-text {
  margin: 0;
  padding: var(--sp-3);
  border-top: 1px solid var(--hairline);
  background: var(--surface-2);
  font-size: 11px;
  line-height: 1.6;
  color: var(--text-dim);
  max-height: 320px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}
.pm-sub {
  margin: var(--sp-4) 0 var(--sp-2);
  font-size: 12px;
  color: var(--text-dim);
  font-weight: 500;
}
.pm-tools {
  display: grid;
  gap: 2px;
}
.pm-tool {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  font-size: 11px;
  color: var(--text-muted);
}
.pm-tool-name {
  font-size: 12px;
  color: var(--text);
  font-weight: 500;
}
.pm-bound {
  margin-left: auto;
}
.pm-tool .badge.read_only {
  background: var(--accent-tint);
  color: var(--accent);
}
.pm-tool .badge.mutating {
  background: var(--warning-tint);
  color: var(--warning);
}
.pm-tool .badge.risky {
  background: var(--danger-tint);
  color: var(--danger);
}
</style>
