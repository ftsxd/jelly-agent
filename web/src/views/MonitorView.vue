<script setup>
import { computed, onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const stats = ref(null)
const loading = ref(true)
const error = ref('')

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    stats.value = await api.stats()
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

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
  box-shadow:
    inset 0 1px 0 rgba(255, 255, 255, 0.04),
    0 6px 20px rgba(0, 0, 0, 0.28);
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
/* Per-KPI hue coding: blue / violet / teal / amber — full-card tint, not
   just the icon chip: gradient wash + tinted border + matching hairline. */
.kpi:nth-child(1) {
  background: linear-gradient(160deg, rgba(110, 139, 255, 0.14), rgba(110, 139, 255, 0.03) 60%), var(--surface);
  border-color: rgba(110, 139, 255, 0.26);
}
.kpi:nth-child(1):hover {
  border-color: rgba(110, 139, 255, 0.5);
}
.kpi:nth-child(2) {
  background: linear-gradient(160deg, rgba(167, 139, 250, 0.14), rgba(167, 139, 250, 0.03) 60%), var(--surface);
  border-color: rgba(167, 139, 250, 0.26);
}
.kpi:nth-child(2):hover {
  border-color: rgba(167, 139, 250, 0.5);
}
.kpi:nth-child(3) {
  background: linear-gradient(160deg, rgba(70, 214, 168, 0.12), rgba(70, 214, 168, 0.03) 60%), var(--surface);
  border-color: rgba(70, 214, 168, 0.26);
}
.kpi:nth-child(3):hover {
  border-color: rgba(70, 214, 168, 0.5);
}
.kpi:nth-child(4) {
  background: linear-gradient(160deg, rgba(240, 180, 84, 0.13), rgba(240, 180, 84, 0.03) 60%), var(--surface);
  border-color: rgba(240, 180, 84, 0.26);
}
.kpi:nth-child(4):hover {
  border-color: rgba(240, 180, 84, 0.5);
}
.kpi:nth-child(1) .kpi-icon {
  background: rgba(110, 139, 255, 0.2);
  color: var(--primary-hover);
}
.kpi:nth-child(2) .kpi-icon {
  background: rgba(167, 139, 250, 0.2);
  color: var(--primary-2);
}
.kpi:nth-child(3) .kpi-icon {
  background: rgba(70, 214, 168, 0.18);
  color: var(--accent);
}
.kpi:nth-child(4) .kpi-icon {
  background: rgba(240, 180, 84, 0.18);
  color: var(--warning);
}
.kpi:nth-child(1)::before {
  background: linear-gradient(90deg, transparent, var(--primary-border), transparent);
}
.kpi:nth-child(2)::before {
  background: linear-gradient(90deg, transparent, var(--primary-2-border), transparent);
}
.kpi:nth-child(3)::before {
  background: linear-gradient(90deg, transparent, var(--accent-border), transparent);
}
.kpi:nth-child(4)::before {
  background: linear-gradient(90deg, transparent, var(--warning-border), transparent);
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
  background: linear-gradient(90deg, var(--primary), var(--primary-2));
  border-radius: 999px;
  min-width: 2px;
  box-shadow:
    inset 0 1px 0 rgba(255, 255, 255, 0.18),
    0 0 10px rgba(110, 139, 255, 0.3);
  transition: width 0.3s ease;
}
/* Tool ranking: categorical palette cycling blue/violet/teal/amber/cyan.
   Each bar runs dark-to-saturated within its own hue, all derived from tokens. */
.bar-row:nth-child(5n + 1) .bar-fill {
  background: linear-gradient(90deg, color-mix(in srgb, var(--primary) 76%, var(--bg)), var(--primary));
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.18), 0 0 10px rgba(110, 139, 255, 0.34);
}
.bar-row:nth-child(5n + 2) .bar-fill {
  background: linear-gradient(90deg, color-mix(in srgb, var(--primary-2) 76%, var(--bg)), var(--primary-2));
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.18), 0 0 10px rgba(167, 139, 250, 0.34);
}
.bar-row:nth-child(5n + 3) .bar-fill {
  background: linear-gradient(90deg, color-mix(in srgb, var(--accent) 76%, var(--bg)), var(--accent));
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.18), 0 0 10px rgba(70, 214, 168, 0.3);
}
.bar-row:nth-child(5n + 4) .bar-fill {
  background: linear-gradient(90deg, color-mix(in srgb, var(--warning) 76%, var(--bg)), var(--warning));
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.18), 0 0 10px rgba(240, 180, 84, 0.3);
}
.bar-row:nth-child(5n + 5) .bar-fill {
  background: linear-gradient(90deg, color-mix(in srgb, var(--cyan) 76%, var(--bg)), var(--cyan));
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.18), 0 0 10px rgba(34, 211, 238, 0.3);
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
  background: linear-gradient(180deg, var(--primary-2), var(--primary) 55%, rgba(110, 139, 255, 0.35));
  border-radius: var(--radius-sm) var(--radius-sm) 0 0;
  box-shadow: 0 0 12px rgba(110, 139, 255, 0.28);
  transition: height 0.3s ease, filter 0.15s ease, box-shadow 0.15s ease;
}
.chart-col:hover .chart-bar {
  filter: brightness(1.25);
  box-shadow: 0 0 20px rgba(167, 139, 250, 0.5);
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
</style>
