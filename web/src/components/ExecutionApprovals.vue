<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { api } from '../api'
import { latestOnly } from '../latest'

const props = defineProps({ session: { type: String, required: true }, refreshKey: { type: Number, default: 0 }, disabled: Boolean, actionable: Boolean })
const emit = defineEmits(['resolve'])
const approvals = ref([]), error = ref('')
const gate = latestOnly()
let timer = null
const labels = { pending: '等待审批', expired: '审批已过期', invalidated: '配置或凭据已改变，审批失效', approved: '已批准，等待执行', consumed: '审批已使用，执行结果未记录，请先核查目标资源', rejected: '已拒绝', abandoned: '执行未开始，审批已失效' }
async function load() {
  clearTimeout(timer)
  const session = props.session
  if (!session) { gate.abandon(); approvals.value = []; return }
  const result = await gate.run(signal => api.executionApprovals(session, signal))
  if (!result.owned) return
  if (result.error) error.value = result.error.message
  else { approvals.value = result.value?.approvals || []; error.value = '' }
  // Consumption precedes execution. Poll for its outcome for a bounded time,
  // including on task/session detail pages where no stream triggers a refresh.
  if (approvals.value.some(a => ['pending', 'approved'].includes(a.state) || (a.state === 'consumed' && !a.outcome && Date.now() < Math.max(a.expires_ms, a.resolved_ms || 0) + 360000))) timer = setTimeout(load, 3000)
}
watch(() => [props.session, props.refreshKey, props.disabled], () => { approvals.value = []; error.value = ''; load() }, { immediate: true })
onUnmounted(() => { clearTimeout(timer); gate.abandon() })
function decision(a, approve) {
  if (props.disabled || a.state !== 'pending') return
  emit('resolve', { id: a.id, approve })
}
function outcome(a) {
  if (a.outcome === 'succeeded') return '执行成功'
  if (a.outcome === 'failed') return '执行失败，请查看工具结果'
  if (a.outcome === 'unknown') return '执行结果未知，请先核查目标资源；不会自动重试'
  return labels[a.state] || a.state
}
// Cards are for what still needs a person: a decision, a run in flight, or an
// outcome nobody can vouch for. Everything settled folds into one line each —
// a session that probed a dozen commands must not bury the one awaiting you.
function live(a) {
  return a.state === 'pending' || a.state === 'approved' || (a.state === 'consumed' && !a.outcome) || a.outcome === 'unknown'
}
const current = computed(() => approvals.value.filter(live))
const settled = computed(() => approvals.value.filter(a => !live(a)))
function tone(a) {
  if (a.outcome === 'succeeded') return 'ok'
  if (a.outcome === 'failed') return 'bad'
  return 'quiet'
}
function when(a) { return new Date(a.resolved_ms || a.created_ms).toLocaleString() }
</script>

<template>
  <section v-if="approvals.length || error" class="approvals" aria-label="命令审批">
    <p v-if="error" role="alert" class="warning">{{ error }} <button class="btn" @click="load">重新加载审批</button></p>
    <article v-for="a in current" :key="a.id" class="approval">
      <div class="heading"><strong>{{ outcome(a) }}</strong><span class="mono">{{ a.agent }} · {{ a.request.profile }}</span></div>
      <p>{{ a.request.purpose }}</p>
      <pre>{{ a.request.command }}</pre>
      <template v-if="a.state === 'pending'">
        <p class="muted">{{ a.reason }}。批准仅对完整命令有效，使用一次即消耗；失败后不会自动重试。</p>
        <p class="muted">有效期至 {{ new Date(a.expires_ms).toLocaleString() }}</p>
        <div class="actions">
          <template v-if="actionable">
            <button class="btn btn-primary" :disabled="disabled" @click="decision(a, true)">批准并执行一次</button>
            <button class="btn" :disabled="disabled" @click="decision(a, false)">拒绝</button>
          </template>
          <RouterLink v-else class="btn" :to="{ path: '/chat', query: { session } }">打开对话审批</RouterLink>
        </div>
      </template>
      <p v-else-if="a.resolved_by" class="muted">处理人 {{ a.resolved_by }} · {{ when(a) }}</p>
      <p v-if="a.exec_id" class="mono muted">{{ a.exec_id }}</p>
    </article>
    <details v-if="settled.length" class="history">
      <summary>已处理的命令审批 {{ settled.length }} 条</summary>
      <details v-for="a in settled" :key="a.id" class="row">
        <summary>
          <span class="state" :class="tone(a)">{{ outcome(a) }}</span>
          <code class="cmd" :title="a.request.command">{{ a.request.command }}</code>
          <span class="muted time">{{ when(a) }}</span>
        </summary>
        <div class="detail">
          <p>{{ a.request.purpose }}</p>
          <pre>{{ a.request.command }}</pre>
          <p class="muted">{{ a.reason }}<span v-if="a.resolved_by"> · 处理人 {{ a.resolved_by }}</span></p>
          <p v-if="a.state === 'expired' || a.state === 'invalidated' || a.state === 'abandoned'" class="warning">请在对话中重新发起命令请求。</p>
          <p v-if="a.exec_id" class="mono muted">{{ a.exec_id }}</p>
        </div>
      </details>
    </details>
  </section>
</template>

<style scoped>
.approvals { max-width: 820px; width: 100%; margin: 0 auto; display: grid; gap: var(--sp-3); }
.approval { padding: var(--sp-4); border: 1px solid var(--warning); border-radius: var(--radius); display: grid; gap: var(--sp-2); background: var(--surface-2); }
.heading, .actions { display: flex; flex-wrap: wrap; justify-content: space-between; gap: var(--sp-2); }
.actions { justify-content: flex-start; }
p { margin: 0; font-size: 12px; line-height: 1.7; }
pre { margin: 0; white-space: pre-wrap; overflow-wrap: anywhere; padding: var(--sp-3); background: var(--surface); border-radius: var(--radius-sm); font-size: 12px; }
.warning { color: var(--warning); }
.history { font-size: 12px; }
.history > summary { cursor: pointer; color: var(--text-muted); padding: var(--sp-1) 0; }
.row { border-top: 1px solid var(--border); }
.row > summary { cursor: pointer; display: flex; align-items: center; gap: var(--sp-2); padding: var(--sp-2) 0; min-width: 0; }
.state { flex: none; }
.state.bad { color: var(--danger); }
.state.quiet { opacity: 0.7; }
.cmd { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.time { flex: none; }
.detail { display: grid; gap: var(--sp-2); padding: 0 0 var(--sp-3); }
</style>
