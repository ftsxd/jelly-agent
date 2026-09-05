<!--
  What the agent has been asked to do, and how far it got.

  Three panes: the tasks on the left, the steps of the selected one top right,
  what it produced bottom right. The shape follows SessionsView — the two-pane
  grid, the hidden list under 820px — because a second layout language for one
  page is how a console starts feeling like several products.

  Nothing here decides anything. Statuses, steps and artifacts all come from
  the server; this file arranges them. The reason is not purity: a status the
  browser inferred from the wording of a result would disagree with the status
  the same run reports through any other surface, and neither would be wrong
  enough to notice.

  Artifact payloads are rendered as text, never as HTML — they are whatever an
  MCP server returned, which is the same reason AgentTimeline never uses v-html.
-->
<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import Icon from '../components/Icon.vue'
import { api } from '../api'
import { latestOnly } from '../latest'
import { absTime, relTime } from '../time'
import { fmtBytes } from '../format'
import {
  anyLive, artifactState, artifactsOfStep, emptyReason, isLive,
  needsAttention, selectionStore, statusOf, stepOfArtifact, typeLabel,
} from '../tasks'

const router = useRouter()

const tasks = ref([])
const total = ref(0)
const loading = ref(true)
const error = ref('')
const filterType = ref('')
const filterStatus = ref('')

const selectedID = ref('')
const detail = ref(null)
const artifacts = ref([])
const detailLoading = ref(false)

const sel = selectionStore()
const openGate = latestOnly()

// Preview state, per artifact. Nothing is fetched until asked for: a task's
// products can be megabytes each, and loading them to draw a list is the
// mistake the delivery budget exists to prevent, made in the browser.
const preview = ref(null)      // { label, text, total, offset, hasMore, expired, error }
const previewLoading = ref(false)
const query = ref('')
const hits = ref(null)

const selectedStep = computed(() => {
  const s = sel.get(selectedID.value).step
  return (detail.value?.steps || []).find((x) => x.id === s) || null
})
const selectedArtifact = computed(() => {
  const a = sel.get(selectedID.value).artifact
  return artifacts.value.find((x) => x.label === a) || null
})
const highlighted = computed(() => {
  const st = selectedStep.value
  return st ? artifactsOfStep(artifacts.value, st.id).map((a) => a.label) : []
})

async function load() {
  loading.value = true
  try {
    const r = await api.tasks({ type: filterType.value, status: filterStatus.value, limit: 100 })
    tasks.value = r.tasks || []
    total.value = r.total || 0
    error.value = ''
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function open(id) {
  selectedID.value = id
  detailLoading.value = true
  detail.value = null
  artifacts.value = []
  preview.value = null
  hits.value = null
  query.value = ''

  const [session, round] = splitID(id)
  const r = await openGate.run(async (signal) => {
    const mine = openGate.current()
    const got = await api.task(session, round, signal)
    if (!openGate.owns(mine)) return null
    detail.value = got.task
    artifacts.value = got.artifacts || []
    return got
  })
  if (!r.owned) return
  if (r.error) error.value = r.error.message
  detailLoading.value = false
}

// Refreshing only the detail, for a task that is still moving. Kept apart from
// open() so a poll does not clear the selection or the open preview under the
// reader's hands.
async function refreshDetail() {
  if (!selectedID.value || !isLive(detail.value)) return
  const [session, round] = splitID(selectedID.value)
  try {
    const got = await api.task(session, round)
    if (selectedID.value !== `${session}/${round}`) return
    detail.value = got.task
    artifacts.value = got.artifacts || []
  } catch { /* a poll that fails is not worth a banner */ }
}

function splitID(id) {
  const i = (id || '').indexOf('/')
  return i < 0 ? [id || '', ''] : [id.slice(0, i), id.slice(i + 1)]
}

function pickStep(step) {
  sel.setStep(selectedID.value, selectedStep.value?.id === step.id ? '' : step.id)
}

async function pickArtifact(a) {
  sel.setArtifact(selectedID.value, a.label)
  preview.value = null
  hits.value = null
  query.value = ''
  // A step is selected alongside, so clicking a product also answers "where
  // did this come from".
  const from = stepOfArtifact(detail.value?.steps, a)
  if (from) sel.setStep(selectedID.value, from.id)

  const st = artifactState(a)
  if (!st.readable) return
  if (a.kind === 'report') {
    preview.value = { label: a.label, text: a.text || '', total: (a.text || '').length, offset: 0, hasMore: false }
    return
  }
  await readMore(a, 0)
}

async function readMore(a, offset) {
  previewLoading.value = true
  const [session] = splitID(selectedID.value)
  try {
    const r = await api.readResult(session, a.call_id, { offset })
    preview.value = {
      label: a.label,
      text: (offset ? (preview.value?.text || '') : '') + (r.data || ''),
      total: r.total || 0,
      offset: r.next_offset || 0,
      hasMore: !!r.has_more,
      upstream: r.upstream_truncated || '',
    }
  } catch (e) {
    // An expired reference is answered with 410 and its own words; anything
    // else is reported as itself rather than as an empty preview.
    preview.value = { label: a.label, text: '', error: e.message }
  } finally {
    previewLoading.value = false
  }
}

async function runSearch() {
  const a = selectedArtifact.value
  if (!a || !query.value.trim()) return
  const [session] = splitID(selectedID.value)
  try {
    hits.value = await api.searchResult(session, a.call_id, query.value.trim(), { context: 1 })
  } catch (e) {
    hits.value = { error: e.message }
  }
}

// Polling, only while something is running.
//
// The only push channel this server has is the chat stream, which is a POST
// that runs a turn — a page cannot subscribe to somebody else's. Polling is
// the mechanism already in use here (MessagingView does the same), so this
// reuses it rather than introducing a third. An idle board sends nothing.
let timer = null
function retime() {
  if (timer) clearInterval(timer)
  timer = null
  if (!anyLive(tasks.value) && !isLive(detail.value)) return
  timer = setInterval(async () => {
    await load()
    await refreshDetail()
  }, 3000)
}
watch([tasks, detail], retime, { deep: false })
onMounted(async () => { await load(); retime() })
onUnmounted(() => timer && clearInterval(timer))

function continueChat(id) {
  router.push({ path: '/chat', query: { session: splitID(id)[0] } })
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>任务</h1>
        <span class="sub muted">{{ total }} 个任务 · 查询监控 / 日志分析 / 日常巡检</span>
      </div>
      <div class="topbar-r">
        <select v-model="filterType" class="input sel" aria-label="按类型筛选" @change="load">
          <option value="">全部类型</option>
          <option value="monitor">监控查询</option>
          <option value="log">日志分析</option>
          <option value="inspection">日常巡检</option>
          <option value="other">其他任务</option>
        </select>
        <select v-model="filterStatus" class="input sel" aria-label="按状态筛选" @change="load">
          <option value="">全部状态</option>
          <option value="running">进行中</option>
          <option value="completed">已完成</option>
          <option value="failed">失败</option>
          <option value="cancelled">已取消</option>
        </select>
        <button class="btn" @click="load"><Icon name="refresh" :size="16" /> 刷新</button>
      </div>
    </header>

    <div class="body" :class="{ picked: !!selectedID }">
      <aside class="list" aria-label="任务列表">
        <div v-if="loading" class="empty"><span class="spinner" /></div>
        <div v-else-if="error && !tasks.length" class="error-bar">
          <Icon name="alert" :size="14" /> {{ error }}
        </div>
        <div v-else-if="!tasks.length" class="empty">
          <Icon name="spark" :size="28" />
          <span class="muted">还没有任务。到「对话」问一句，或让周期任务跑一次。</span>
        </div>

        <div
          v-for="t in tasks"
          :key="t.id"
          class="task"
          :class="{ active: t.id === selectedID, attn: needsAttention(t) }"
          role="button"
          :tabindex="0"
          :aria-current="t.id === selectedID ? 'true' : undefined"
          @click="open(t.id)"
          @keydown.enter="open(t.id)"
          @keydown.space.prevent="open(t.id)"
        >
          <div class="task-top">
            <span class="task-title">{{ t.title }}</span>
          </div>
          <div class="task-meta">
            <span class="badge">{{ typeLabel(t.type) }}</span>
            <span class="badge" :class="'tone-' + statusOf(t.status).tone">
              <Icon :name="statusOf(t.status).icon" :size="11" />
              {{ statusOf(t.status).label }}
            </span>
            <span class="muted time" :title="absTime(Math.floor((t.ended_at || t.started_at || 0) / 1000))">
              {{ relTime(Math.floor((t.ended_at || t.started_at || 0) / 1000)) }}
            </span>
          </div>
          <div v-if="needsAttention(t)" class="task-attn">
            <Icon name="alert" :size="12" /> 需要处理
          </div>
        </div>
      </aside>

      <section class="detail">
        <!-- Narrow screens show one pane at a time, so there has to be a way
             back. Hiding the list outright — which is what the sessions page
             does — leaves a phone unable to pick a task at all. -->
        <button class="btn btn-sm back" @click="selectedID = ''">
          <Icon name="chevron" :size="14" /> 返回任务列表
        </button>

        <div v-if="detailLoading" class="empty"><span class="spinner" /> 加载任务…</div>
        <div v-else-if="!detail" class="empty">
          <Icon name="doc" :size="28" />
          <span class="muted">{{ emptyReason(null, []) }}</span>
        </div>

        <template v-else>
          <div class="detail-head">
            <div>
              <h2 class="d-title">{{ detail.title }}</h2>
              <div class="d-sub muted">
                <span>{{ typeLabel(detail.type) }}</span>
                <span class="mono">{{ detail.session_id }}</span>
                <span v-if="detail.started_at">
                  {{ absTime(Math.floor(detail.started_at / 1000)) }}
                </span>
              </div>
            </div>
            <div class="detail-actions">
              <span class="badge" :class="'tone-' + statusOf(detail.status).tone">
                <Icon :name="statusOf(detail.status).icon" :size="12" />
                {{ statusOf(detail.status).label }}
              </span>
              <button class="btn btn-sm" @click="continueChat(detail.id)">继续对话</button>
            </div>
          </div>

          <div v-if="detail.error" class="error-bar">
            <Icon name="alert" :size="14" /> {{ detail.error }}
          </div>

          <!-- 执行流程 -->
          <div class="block">
            <div class="block-head">
              <span class="block-title">执行流程</span>
              <span class="muted tiny">{{ (detail.steps || []).length }} 步</span>
            </div>
            <ol v-if="(detail.steps || []).length" class="steps">
              <li
                v-for="s in detail.steps"
                :key="s.id"
                class="step"
                :class="{ on: selectedStep?.id === s.id, ['s-' + statusOf(s.status).tone]: true }"
                role="button"
                :tabindex="0"
                :aria-expanded="selectedStep?.id === s.id"
                @click="pickStep(s)"
                @keydown.enter="pickStep(s)"
              >
                <span class="step-n mono">{{ String(s.index + 1).padStart(2, '0') }}</span>
                <span class="step-label">{{ s.label }}</span>
                <span class="step-status">
                  <Icon :name="statusOf(s.status).icon" :size="11" />
                  {{ statusOf(s.status).label }}
                </span>
                <span v-if="s.calls" class="muted tiny">{{ s.calls }} 次调用</span>
              </li>
            </ol>
            <div v-else class="empty small"><span class="muted">这个任务没有执行步骤</span></div>

            <div v-if="selectedStep" class="step-detail">
              <div v-if="selectedStep.progress" class="sd-progress">{{ selectedStep.progress }}</div>
              <div v-if="selectedStep.error" class="error-bar">
                <Icon name="alert" :size="14" /> {{ selectedStep.error }}
              </div>
              <div class="sd-row muted tiny">
                <span v-if="selectedStep.started_at">
                  开始 {{ absTime(Math.floor(selectedStep.started_at / 1000)) }}
                </span>
                <span v-if="selectedStep.ended_at">
                  结束 {{ absTime(Math.floor(selectedStep.ended_at / 1000)) }}
                </span>
                <span v-if="selectedStep.agent">Agent {{ selectedStep.agent }}</span>
              </div>
              <table v-if="(selectedStep.tools || []).length" class="tools">
                <thead>
                  <tr><th>工具</th><th>结果</th><th>耗时</th><th>证据</th></tr>
                </thead>
                <tbody>
                  <tr v-for="(tl, i) in selectedStep.tools" :key="i">
                    <td class="mono">{{ tl.name }}</td>
                    <td>
                      <span v-if="tl.pending" class="muted">执行中…</span>
                      <span v-else-if="tl.ok" class="ok-text"><Icon name="check" :size="11" /> 成功</span>
                      <span v-else class="bad-text"><Icon name="alert" :size="11" /> {{ tl.error || '失败' }}</span>
                    </td>
                    <td class="mono">{{ tl.duration_ms ? tl.duration_ms + ' ms' : '—' }}</td>
                    <td class="mono">
                      {{ tl.evidence_id || '—' }}
                      <span v-if="tl.bytes" class="muted tiny">{{ fmtBytes(tl.bytes) }}</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>

          <!-- 任务产物 -->
          <div class="block">
            <div class="block-head">
              <span class="block-title">任务产物</span>
              <span class="muted tiny">{{ artifacts.length }} 项</span>
            </div>
            <div v-if="!artifacts.length" class="empty small">
              <span class="muted">{{ emptyReason(detail, artifacts) }}</span>
            </div>
            <div
              v-for="a in artifacts"
              :key="a.label"
              class="art"
              :class="{ on: selectedArtifact?.label === a.label, lit: highlighted.includes(a.label) }"
              role="button"
              :tabindex="0"
              @click="pickArtifact(a)"
              @keydown.enter="pickArtifact(a)"
            >
              <div class="art-top">
                <span class="art-name mono">{{ a.name }}</span>
                <span class="badge" :class="'tone-' + (artifactState(a).tone || 'muted')">
                  {{ artifactState(a).badge }}
                </span>
                <span v-if="a.step" class="badge">来自步骤 {{ a.step }}</span>
              </div>
              <div class="art-meta muted tiny">
                <span class="mono">{{ a.label }}</span>
                <span>{{ fmtBytes(a.bytes) || a.bytes + ' B' }}</span>
                <span v-if="a.lines">{{ a.lines }} 行</span>
                <span v-if="a.at">{{ relTime(Math.floor(a.at / 1000)) }}</span>
                <span v-if="a.upstream === 'yes'">上游已自行截断</span>
              </div>
              <div v-if="artifactState(a).note" class="art-note">{{ artifactState(a).note }}</div>
            </div>

            <div v-if="selectedArtifact && artifactState(selectedArtifact).readable" class="art-view">
              <div v-if="selectedArtifact.kind !== 'report'" class="art-search">
                <Icon name="search" :size="14" />
                <input
                  v-model="query"
                  class="input"
                  placeholder="在完整内容里按正则搜索，不必整份加载"
                  @keydown.enter="runSearch"
                  @keydown.esc="query = ''"
                />
                <button class="btn btn-sm" @click="runSearch">搜索</button>
              </div>

              <div v-if="hits?.error" class="error-bar"><Icon name="alert" :size="14" /> {{ hits.error }}</div>
              <div v-else-if="hits" class="hits">
                <div class="hits-head muted tiny">
                  命中 {{ hits.total_matches }} 处{{ hits.total_exact ? '' : '（至少）' }} ·
                  共 {{ hits.lines }} 行 · 显示 {{ (hits.hits || []).length }} 条
                </div>
                <pre v-for="(h, i) in hits.hits" :key="i" class="hit mono"><span class="hit-n">{{ h.line }}</span>{{ h.text }}</pre>
              </div>

              <div v-if="preview?.error" class="error-bar"><Icon name="alert" :size="14" /> {{ preview.error }}</div>
              <template v-else-if="preview">
                <pre class="art-body mono">{{ preview.text }}</pre>
                <div class="art-foot muted tiny">
                  已读 {{ fmtBytes(preview.offset || preview.total) }} / {{ fmtBytes(preview.total) }}
                  <button v-if="preview.hasMore" class="btn btn-sm" :disabled="previewLoading"
                          @click="readMore(selectedArtifact, preview.offset)">继续读取</button>
                </div>
              </template>
              <div v-else-if="previewLoading" class="empty small"><span class="spinner" /></div>
            </div>
            <div v-else-if="selectedArtifact" class="art-view">
              <div class="error-bar">
                <Icon name="alert" :size="14" /> {{ artifactState(selectedArtifact).note }}
              </div>
            </div>
          </div>
        </template>
      </section>
    </div>
  </div>
</template>

<style scoped>
.view { display: flex; flex-direction: column; height: 100%; }
.topbar { display: flex; align-items: center; justify-content: space-between;
          gap: var(--sp-3); padding: var(--sp-4) var(--sp-5);
          border-bottom: 1px solid var(--border); }
.topbar h1 { font-size: 18px; }
.topbar-l { display: flex; align-items: baseline; gap: var(--sp-3); }
.topbar-r { display: flex; gap: var(--sp-2); align-items: center; }
.sub { font-size: 12px; }
.sel { width: auto; padding-right: var(--sp-4); }
.tiny { font-size: 11px; }

.body { flex: 1; display: grid; grid-template-columns: 320px 1fr; overflow: hidden; }
.list { border-right: 1px solid var(--border); overflow-y: auto; padding: var(--sp-3);
        display: flex; flex-direction: column; gap: var(--sp-2); }
.detail { overflow-y: auto; padding: var(--sp-5); display: flex; flex-direction: column; gap: var(--sp-4); }

.task { display: flex; flex-direction: column; gap: var(--sp-2); padding: var(--sp-3);
        border: 1px solid var(--border); border-radius: var(--radius-sm);
        background: var(--surface); cursor: pointer;
        transition: border-color .15s ease, background .15s ease, transform .18s ease; }
.task:hover { background: var(--surface-2); border-color: var(--border-strong); transform: translateY(-1px); }
.task.active { border-color: var(--primary-border); background: var(--primary-tint); }
.task.attn { border-color: var(--warning-border); }
.task-title { display: -webkit-box; -webkit-line-clamp: 2; line-clamp: 2;
              -webkit-box-orient: vertical; overflow: hidden; }
.task-meta { display: flex; align-items: center; gap: var(--sp-2); flex-wrap: wrap; font-size: 11px; }
.task-meta .time { margin-left: auto; }
.task-attn { display: flex; align-items: center; gap: 4px; font-size: 11px; color: var(--warning); }

/* Status tones. The colour is never alone — every badge carries an icon and a
   word, so the state survives a monochrome screen and colour-blind readers. */
.tone-ok { background: var(--accent-tint); color: var(--accent); }
.tone-bad { background: var(--danger-tint); color: var(--danger); }
.tone-warn { background: var(--warning-tint); color: var(--warning); }
.tone-run { background: var(--primary-tint); color: var(--primary); }
.tone-muted { color: var(--text-muted); }
.badge { display: inline-flex; align-items: center; gap: 3px; }

.detail-head { display: flex; align-items: flex-start; justify-content: space-between;
               gap: var(--sp-3); padding-bottom: var(--sp-3); border-bottom: 1px solid var(--border); }
.d-title { font-size: 16px; }
.d-sub { display: flex; gap: var(--sp-3); flex-wrap: wrap; font-size: 12px; margin-top: 4px; }
.detail-actions { display: flex; align-items: center; gap: var(--sp-2); flex-shrink: 0; }

.block { display: flex; flex-direction: column; gap: var(--sp-2); }
.block-head { display: flex; align-items: baseline; justify-content: space-between; }
.block-title { font-size: 13px; font-weight: 600; }

.steps { display: flex; gap: var(--sp-2); list-style: none; margin: 0; padding: 0;
         overflow-x: auto; padding-bottom: var(--sp-1); }
.step { flex: 0 0 auto; min-width: 148px; display: flex; flex-direction: column; gap: 4px;
        padding: var(--sp-3); border: 1px solid var(--border); border-radius: var(--radius-sm);
        background: var(--surface); cursor: pointer; }
.step:hover { border-color: var(--border-strong); }
.step.on { border-color: var(--primary); background: var(--primary-tint); }
.step-n { font-size: 11px; color: var(--text-muted); }
.step-label { font-size: 13px; }
.step-status { display: inline-flex; align-items: center; gap: 3px; font-size: 11px; }
.s-ok .step-status { color: var(--accent); }
.s-bad .step-status { color: var(--danger); }
.s-run .step-status { color: var(--primary); }
.s-warn .step-status { color: var(--warning); }

.step-detail { display: flex; flex-direction: column; gap: var(--sp-2);
               padding: var(--sp-3); border: 1px solid var(--hairline);
               border-radius: var(--radius-sm); background: var(--surface-2); }
.sd-progress { font-size: 13px; }
.sd-row { display: flex; gap: var(--sp-3); flex-wrap: wrap; }
.tools { width: 100%; border-collapse: collapse; font-size: 12px; }
.tools th { text-align: left; font-weight: 500; color: var(--text-muted);
            padding: 4px 8px 4px 0; border-bottom: 1px solid var(--hairline); }
.tools td { padding: 4px 8px 4px 0; border-bottom: 1px solid var(--hairline); vertical-align: top; }
.ok-text { color: var(--accent); display: inline-flex; align-items: center; gap: 3px; }
.bad-text { color: var(--danger); display: inline-flex; align-items: center; gap: 3px; }

.art { display: flex; flex-direction: column; gap: 4px; padding: var(--sp-3);
       border: 1px solid var(--border); border-radius: var(--radius-sm);
       background: var(--surface); cursor: pointer; }
.art:hover { border-color: var(--border-strong); }
.art.on { border-color: var(--primary); background: var(--primary-tint); }
.art.lit { border-color: var(--primary-border); }
.art-top { display: flex; align-items: center; gap: var(--sp-2); flex-wrap: wrap; }
.art-name { font-size: 13px; }
.art-meta { display: flex; gap: var(--sp-3); flex-wrap: wrap; }
.art-note { font-size: 11px; color: var(--text-dim); }

.art-view { display: flex; flex-direction: column; gap: var(--sp-2);
            padding: var(--sp-3); border: 1px solid var(--hairline);
            border-radius: var(--radius-sm); background: var(--surface-2); }
.art-search { display: flex; align-items: center; gap: var(--sp-2); }
.art-body { margin: 0; max-height: 320px; overflow: auto; font-size: 12px;
            line-height: 1.55; white-space: pre-wrap; word-break: break-word; color: var(--text-dim); }
.art-foot { display: flex; align-items: center; gap: var(--sp-3); }
.hits { display: flex; flex-direction: column; gap: 2px; }
.hit { margin: 0; font-size: 12px; white-space: pre-wrap; word-break: break-word; color: var(--text-dim); }
.hit-n { display: inline-block; min-width: 52px; color: var(--text-muted); }

.empty.small { padding: var(--sp-4); }
.error-bar { display: flex; align-items: center; gap: var(--sp-2);
             padding: var(--sp-2) var(--sp-3); background: var(--danger-tint);
             color: var(--danger); border-radius: var(--radius-sm); font-size: 13px; }

.back { display: none; align-self: flex-start; }

@media (max-width: 820px) {
  /* One pane at a time, and both reachable. The list is the landing state and
     picking a task swaps to its detail, which carries a way back — rather
     than hiding the list unconditionally, which makes a task unpickable on a
     phone. */
  .body { grid-template-columns: 1fr; }
  .body.picked .list { display: none; }
  .body:not(.picked) .detail { display: none; }
  .list { border-right: 0; }
  .back { display: inline-flex; }
  .steps { flex-direction: column; }
  .step { min-width: 0; }
  .topbar { flex-wrap: wrap; }
}
</style>
