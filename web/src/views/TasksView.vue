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
import { sendOnEnter } from '../ime'
import { latestOnly } from '../latest'
import { absTime, relTime } from '../time'
import { fmtBytes, prettyJSON } from '../format'
import { renderMarkdown } from '../markdown'
import {
  anyLive, artifactState, artifactsOfStep, emptyReason, handoverAt, isLive,
  loadTaskList, needsAttention, resultOf, selectionStore, statusOf,
  stepOfArtifact, stepSummary, toggleStep, typeLabel,
} from '../tasks'

const router = useRouter()

const tasks = ref([])
const total = ref(0)
// Whether that count is the number of tasks or a floor. The list reads back
// through the sessions only as far as it needs to, and stops at a ceiling when
// a filter matches nothing — saying "42" in that case would claim a search
// that never finished.
const totalExact = ref(true)
const scanned = ref(0)
const sessionsTotal = ref(0)
const skippedChat = ref(0)
const loading = ref(true)
const error = ref('')
const filterType = ref('')
const filterStatus = ref('')

const selectedID = ref('')
const detail = ref(null)
const artifacts = ref([])
const results = ref({})
const detailLoading = ref(false)

// The current selection lives in refs, not in the store.
//
// The first version kept it in a plain Map and read it through a computed,
// which never re-ran — a Map is not reactive — so clicking a step did nothing
// at all. The store now only remembers what each task had picked, so switching
// away and back returns to it.
const stepID = ref('')
const artifactID = ref('')
const sel = selectionStore()
const openGate = latestOnly()
// The list is owned too. Filters are two selects and a poll fires every three
// seconds, so a slow response for 全部 could land after a fast one for 失败 and
// repopulate the board with rows the filter excludes — with the filter still
// reading 失败, which makes it look like the filter is broken rather than late.
const listGate = latestOnly()

// What is open in the product pane, addressed by its handle.
//
// A handle, not an artifact object: most tool results are too small to be
// listed as products of the run, and opening one from its step used to leave
// the pane's other controls — 继续读取 and 搜索 — looking at `selectedArtifact`,
// which is null for exactly those. They searched the wrong thing or nothing.
const preview = ref(null)
const previewLoading = ref(false)
const query = ref('')
const hits = ref(null)
// Both requests are owned: switching product while one is in flight must not
// let the abandoned answer render under the new selection. Separate gates, so
// starting a search does not cancel a 继续读取 that is still arriving.
const readGate = latestOnly()
const searchGate = latestOnly()
// The handle the pane is showing, whether it came from the shelf or a step.
const openRef = computed(() => preview.value?.ref || artifactID.value)

const steps = computed(() => detail.value?.steps || [])
const selectedStep = computed(() => steps.value.find((s) => s.id === stepID.value) || null)
const selectedArtifact = computed(() => artifacts.value.find((a) => a.label === artifactID.value) || null)
const highlighted = computed(() =>
  selectedStep.value ? artifactsOfStep(artifacts.value, selectedStep.value.id).map((a) => a.label) : [],
)

// Remember the selection whenever it moves, so returning to a task returns to
// where the reader was.
watch([stepID, artifactID], () => {
  if (selectedID.value) sel.set(selectedID.value, { step: stepID.value, artifact: artifactID.value })
})

// The ordering rule lives in tasks.js so it can be tested; this supplies the
// request and the three places its answer lands.
async function load() {
  await loadTaskList(
    listGate,
    (signal) => api.tasks({ type: filterType.value, status: filterStatus.value, limit: 100 }, signal),
    {
      loading: (busy) => { loading.value = busy },
      error: (message) => { error.value = message },
      data: (r) => {
        tasks.value = r.tasks || []
        total.value = r.total || 0
        totalExact.value = r.total_exact !== false
        scanned.value = r.scanned_sessions || 0
        sessionsTotal.value = r.sessions_total || 0
        skippedChat.value = r.skipped_chat || 0
        error.value = ''
      },
    },
  )
}

async function open(id) {
  selectedID.value = id
  detailLoading.value = true
  detail.value = null
  artifacts.value = []
  results.value = {}
  clearPreview()

  const remembered = sel.get(id)
  stepID.value = remembered.step
  artifactID.value = remembered.artifact

  const [session, round] = splitID(id)
  const r = await openGate.run(async (signal) => {
    const mine = openGate.current()
    const got = await api.task(session, round, signal)
    if (!openGate.owns(mine)) return null
    detail.value = got.task
    artifacts.value = got.artifacts || []
    results.value = got.results || {}
    return got
  })
  if (!r.owned) return
  if (r.error) error.value = r.error.message
  detailLoading.value = false
}

// Refreshing a running task's detail without clearing the selection or the
// open preview under the reader's hands.
async function refreshDetail() {
  if (!selectedID.value || !isLive(detail.value)) return
  const [session, round] = splitID(selectedID.value)
  try {
    const got = await api.task(session, round)
    if (selectedID.value !== `${session}/${round}`) return
    detail.value = got.task
    artifacts.value = got.artifacts || []
    results.value = got.results || {}
  } catch { /* a poll that fails is not worth a banner */ }
}

function splitID(id) {
  const i = (id || '').indexOf('/')
  return i < 0 ? [id || '', ''] : [id.slice(0, i), id.slice(i + 1)]
}

function pickStep(step) {
  stepID.value = toggleStep(stepID.value, step.id)
}

async function pickArtifact(a) {
  artifactID.value = a.label
  clearPreview()
  const from = stepOfArtifact(steps.value, a)
  if (from) stepID.value = from.id
  if (!artifactState(a).readable) return
  await readMore(a.label, 0)
}

// clearPreview empties the product pane and disowns anything still in flight.
//
// Both halves, always together. Emptying without disowning leaves the previous
// request able to write into the pane it no longer belongs to; disowning
// without emptying leaves the last product's bytes under the new selection's
// heading. The paths that fetch nothing — an expired product, a result that
// was never stored, switching task — are exactly the ones that used to do
// neither.
function clearPreview() {
  readGate.abandon()
  searchGate.abandon()
  preview.value = null
  previewLoading.value = false
  hits.value = null
  query.value = ''
}

// Reading one tool call's result from inside a step, for the results that are
// too small to be listed as products of the run but are exactly what someone
// clicking that step wants to see.
async function openCallResult(tool) {
  const found = resultOf(results.value, tool)
  if (!found) return
  artifactID.value = found.label
  clearPreview()
  if (!found.retrievable) {
    preview.value = {
      ref: found.label, text: '',
      note: found.expired
        ? '结果已过期，请重新查询'
        : '这次返回没有留在结果存储里，只能看到调用当时记录的摘要',
    }
    return
  }
  await readMore(found.label, 0)
}

async function readMore(ref, offset) {
  if (!ref) return
  previewLoading.value = true
  const [session] = splitID(selectedID.value)
  const carry = offset ? (preview.value?.text || '') : ''
  const r = await readGate.run((signal) => api.readResult(session, ref, { offset }, signal))
  if (!r.owned) return // superseded: another product is open, leave it alone
  previewLoading.value = false
  if (r.error) {
    // 410 carries its own words for expiry, 404 for a reference that never
    // existed. Either way the message is shown as itself, never as an empty
    // preview that reads like "the tool returned nothing".
    preview.value = { ref, text: '', error: r.error.message }
    return
  }
  preview.value = {
    ref,
    text: carry + (r.value.data || ''),
    total: r.value.total || 0,
    offset: r.value.next_offset || 0,
    hasMore: !!r.value.has_more,
    upstream: r.value.upstream_truncated || '',
  }
}

// Enter searches — unless it belongs to the input method. See ime.js.
const searchEnter = sendOnEnter(() => runSearch())

async function runSearch() {
  const ref = openRef.value
  const q = query.value.trim()
  if (!ref || !q) return
  const [session] = splitID(selectedID.value)
  const r = await searchGate.run((signal) => api.searchResult(session, ref, q, { context: 1 }, signal))
  if (!r.owned || openRef.value !== ref) return
  hits.value = r.error ? { error: r.error.message } : r.value
}

// Polling, only while something is running.
//
// The only push channel this server has is the chat stream, which is a POST
// that runs a turn — a page cannot subscribe to somebody else's. Polling is
// the mechanism already in use here, so this reuses it rather than adding a
// third. An idle board sends nothing.
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

// Continuing a task carries its id, so the follow-up joins this task instead
// of opening a second one that tells half the story.
function continueChat(id) {
  router.push({ path: '/chat', query: { session: splitID(id)[0], task: id } })
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>任务</h1>
        <span class="sub muted">
          {{ totalExact ? '' : '至少 ' }}{{ total }} 个任务 · 查询监控 / 日志分析 / 日常巡检
          <template v-if="!totalExact">
            · 已回溯 {{ scanned }}/{{ sessionsTotal }} 个会话，更早的请缩小筛选范围
          </template>
        </span>
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
          <span v-if="skippedChat" class="muted tiny">
            另有 {{ skippedChat }} 次普通对话没有列入——它们没有执行任何操作，在「会话」页查看
          </span>
        </div>
        <div v-else-if="skippedChat" class="hint-bar muted tiny">
          {{ skippedChat }} 次普通对话未列入（没有工具调用，也没有产物）
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
                v-for="(s, i) in detail.steps"
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
                <!-- 换手：协调者判断这件事不归自己做的那一刻。转交本身不产生
                     步骤（它不干活），所以标在接手方的第一张卡上。 -->
                <span v-if="handoverAt(detail.steps, i)" class="step-agent">
                  <Icon name="bot" :size="11" /> {{ handoverAt(detail.steps, i) }}
                </span>
                <span class="step-label">{{ s.label }}</span>
                <span class="step-status">
                  <Icon :name="statusOf(s.status).icon" :size="11" />
                  {{ statusOf(s.status).label }}
                </span>
                <span v-if="stepSummary(s)" class="step-sum muted tiny">{{ stepSummary(s) }}</span>
              </li>
            </ol>
            <div v-else class="empty small"><span class="muted">这个任务没有执行步骤</span></div>

            <!-- 点击步骤后的真实执行详情 -->
            <div v-if="selectedStep" class="step-detail">
              <div class="sd-head">
                <span class="sd-name">{{ selectedStep.label }}</span>
                <span class="badge" :class="'tone-' + statusOf(selectedStep.status).tone">
                  <Icon :name="statusOf(selectedStep.status).icon" :size="11" />
                  {{ statusOf(selectedStep.status).label }}
                </span>
              </div>
              <div v-if="selectedStep.note" class="sd-note">{{ selectedStep.note }}</div>
              <div v-if="selectedStep.error" class="error-bar">
                <Icon name="alert" :size="14" /> {{ selectedStep.error }}
              </div>
              <div class="sd-row muted tiny">
                <span v-if="selectedStep.started_at">开始 {{ absTime(Math.floor(selectedStep.started_at / 1000)) }}</span>
                <span v-if="selectedStep.ended_at">结束 {{ absTime(Math.floor(selectedStep.ended_at / 1000)) }}</span>
                <span v-if="selectedStep.agent">Agent {{ selectedStep.agent }}</span>
                <span v-if="selectedStep.calls">{{ selectedStep.calls }} 次工具调用</span>
              </div>

              <!-- 每次调用：名称、状态、耗时、脱敏后的参数、结果概况、证据 -->
              <div v-for="(tl, i) in selectedStep.tools" :key="i" class="call">
                <div class="call-head">
                  <span class="mono call-name">{{ tl.name }}</span>
                  <span v-if="tl.pending" class="muted tiny">执行中…</span>
                  <span v-else-if="tl.ok" class="ok-text tiny"><Icon name="check" :size="11" /> 成功</span>
                  <span v-else class="bad-text tiny"><Icon name="alert" :size="11" /> 失败</span>
                  <span v-if="tl.duration_ms" class="mono muted tiny">{{ tl.duration_ms }} ms</span>
                  <span class="call-sp" />
                  <button
                    v-if="resultOf(results, tl)"
                    class="btn btn-sm"
                    @click="openCallResult(tl)"
                  >查看结果</button>
                </div>
                <div v-if="tl.args" class="call-args mono">{{ prettyJSON(tl.args) }}</div>
                <div v-if="tl.error" class="error-bar">{{ tl.error }}</div>
                <div v-else-if="tl.summary" class="call-sum">{{ tl.summary }}</div>
                <div class="call-meta muted tiny">
                  <span v-if="tl.evidence_id" class="mono">证据 {{ tl.evidence_id }}</span>
                  <span v-if="tl.bytes">{{ fmtBytes(tl.bytes) }}</span>
                  <span v-if="tl.lines">{{ tl.lines }} 行</span>
                  <span v-if="tl.withheld" class="warn-text">未进上下文</span>
                  <span v-else-if="tl.truncated" class="warn-text">已截断</span>
                  <span v-if="resultOf(results, tl)?.expired" class="bad-text">已过期</span>
                  <span v-else-if="resultOf(results, tl)?.retrievable" class="ok-text">可重读</span>
                  <span v-else class="muted">未保存</span>
                </div>
              </div>
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

            <div v-if="preview || previewLoading" class="art-view">
              <div class="art-search">
                <Icon name="search" :size="14" />
                <input
                  v-model="query"
                  class="input"
                  placeholder="在完整内容里按正则搜索，不必整份加载"
                  @keydown.enter="searchEnter.keydown"
                  @compositionend="searchEnter.compositionend"
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
              <div v-else-if="preview?.note" class="notice-bar"><Icon name="alert" :size="14" /> {{ preview.note }}</div>
              <template v-else-if="preview">
                <pre class="art-body mono">{{ preview.text }}</pre>
                <div class="art-foot muted tiny">
                  已读 {{ fmtBytes(preview.offset || preview.total) }} / {{ fmtBytes(preview.total) }}
                  <button v-if="preview.hasMore" class="btn btn-sm" :disabled="previewLoading"
                          @click="readMore(preview.ref, preview.offset)">继续读取</button>
                </div>
              </template>
              <div v-else-if="previewLoading" class="empty small"><span class="spinner" /></div>
            </div>
            <div v-else-if="selectedArtifact && !artifactState(selectedArtifact).readable" class="art-view">
              <div class="notice-bar">
                <Icon name="alert" :size="14" /> {{ artifactState(selectedArtifact).note }}
              </div>
              <div v-if="selectedArtifact.summary" class="art-body mono">{{ selectedArtifact.summary }}</div>
            </div>
          </div>
          <!-- 最终回复：用户真正收到的那条，逐字保留 -->
          <div class="block">
            <div class="block-head">
              <span class="block-title">最终回复</span>
              <span v-if="detail.reply" class="muted tiny">用户实际看到的回答</span>
            </div>
            <!-- 与对话页同一个渲染器和同一套 .md 样式：这是同一段文本，
                 用户在对话里看到的和在任务详情里回看的必须长一样。 -->
            <div v-if="detail.reply" class="reply md" v-html="renderMarkdown(detail.reply)"></div>
            <div v-else-if="detail.status === 'failed'" class="error-bar">
              <Icon name="alert" :size="14" />
              {{ detail.error || '任务失败，没有产生最终回复' }}
            </div>
            <div v-else class="empty small"><span class="muted">这个任务还没有最终回复</span></div>
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
.detail { overflow-y: auto; padding: var(--sp-5); display: flex; flex-direction: column; gap: var(--sp-4);
          /* 没有这一行，横向滚动永远不会发生。grid item 的 min-width 默认是
             auto，也就是「不小于内容的最小宽度」，而流程条里的卡片是
             flex: 0 0 auto、不可收缩的，于是整条的宽度成了这一栏的下限：
             栏被撑破、后面的步骤被裁掉，.steps 上的 overflow-x 根本没机会
             生效。 */
          min-width: 0; }

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

.block { min-width: 0; }
.steps { display: flex; gap: var(--sp-2); list-style: none; margin: 0; padding: 0;
         overflow-x: auto; overscroll-behavior-x: contain;
         /* 滚动条常驻可见，否则触控板用户看不出这里还能往右滑。 */
         scrollbar-width: thin;
         padding-bottom: var(--sp-2); }
.step { flex: 0 0 auto; min-width: 148px; max-width: 240px;
        display: flex; flex-direction: column; gap: 4px;
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
.hint-bar { padding: var(--sp-2) var(--sp-3); }
.sd-head { display: flex; align-items: center; gap: var(--sp-2); }
.sd-name { font-size: 13px; font-weight: 600; }
.sd-note { font-size: 13px; color: var(--text-dim); }
.step-sum { display: -webkit-box; -webkit-line-clamp: 2; line-clamp: 2;
            -webkit-box-orient: vertical; overflow: hidden;
            /* 卡片有 max-width，长句子必须能断行，否则一个不换行的
               URL 或指标名照样把卡片顶宽。 */
            overflow-wrap: anywhere; }
.step-agent { font-size: 11px; color: var(--primary); display: inline-flex;
              align-items: center; gap: 3px; }
.call { display: flex; flex-direction: column; gap: 4px; padding: var(--sp-2) 0;
        border-top: 1px solid var(--hairline); }
.call-head { display: flex; align-items: center; gap: var(--sp-2); }
.call-name { font-size: 12px; }
.call-sp { flex: 1; }
.call-args { font-size: 11px; color: var(--text-muted); background: var(--surface);
             padding: var(--sp-2); border-radius: var(--radius-sm);
             max-height: 120px; overflow: auto; white-space: pre-wrap;
             word-break: break-word; margin: 0; }
.call-sum { font-size: 12px; color: var(--text-dim); }
.call-meta { display: flex; gap: var(--sp-3); flex-wrap: wrap; }
.warn-text { color: var(--warning); }
.notice-bar { display: flex; align-items: center; gap: var(--sp-2);
              padding: var(--sp-2) var(--sp-3); background: var(--warning-tint);
              color: var(--warning); border-radius: var(--radius-sm); font-size: 13px; }
.reply { font-size: 14px; line-height: 1.7;
         /* 没有 pre-wrap：渲染后的 HTML 自带块级元素，再按原样保留换行
            会把标签之间的换行也变成空行。 */
         word-break: break-word; padding: var(--sp-3);
         border: 1px solid var(--hairline); border-radius: var(--radius-sm);
         background: var(--surface); }
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
