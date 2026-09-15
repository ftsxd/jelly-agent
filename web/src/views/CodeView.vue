<script setup>
// Code directories: which trees the agent's read-only file tools may reach.
// Deliberately its own page rather than a field on the sandbox card — the
// sandbox governs what SCRIPTS may touch, this governs what the MODEL may read,
// and the two answer to different mechanisms.
import { onMounted, reactive, ref, computed } from 'vue'
import Icon from '../components/Icon.vue'
import CodeProjects from '../components/CodeProjects.vue'
import { api } from '../api'

const state = reactive({ roots: [], tools: [], toolsEnabled: false, skipDirs: [] })
const draft = ref([])        // the editable copy; state.roots stays as loaded
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const notice = ref('')
const newPath = ref('')

const dirty = computed(() => JSON.stringify(draft.value) !== JSON.stringify(state.roots.map((r) => r.path)))
const usable = computed(() => state.roots.filter((r) => r.exists).length)

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await api.files()
    state.roots = res.roots || []
    state.tools = res.tools || []
    state.toolsEnabled = !!res.tools_enabled
    state.skipDirs = res.skip_dirs || []
    draft.value = state.roots.map((r) => r.path)
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function addPath() {
  const p = newPath.value.trim()
  if (!p) return
  if (draft.value.includes(p)) {
    error.value = '这个目录已经在列表里了'
    return
  }
  draft.value.push(p)
  newPath.value = ''
  error.value = ''
}

function removePath(i) {
  draft.value.splice(i, 1)
}

function revert() {
  draft.value = state.roots.map((r) => r.path)
  error.value = ''
  notice.value = ''
}

async function save() {
  if (saving.value) return
  saving.value = true
  error.value = ''
  notice.value = ''
  try {
    await api.setFiles(draft.value)
    notice.value = '已保存（即时热重载）'
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    saving.value = false
  }
}

// Pair each draft row with what the server last reported about it, so a freshly
// typed path shows as "未检查" rather than borrowing another row's status.
function statusOf(path) {
  return state.roots.find((r) => r.path === path)
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>代码</h1>
        <span class="muted sub">项目拉取与 Agent 访问授权</span>
      </div>

    </header>

    <div class="body">
      <CodeProjects />
      <details class="legacy-roots">
        <summary>高级：全局共享代码目录（所有 Agent 可读）</summary>
      <div v-if="notice" class="notice-bar"><Icon name="check" :size="16" /> {{ notice }}</div>
      <div v-if="error" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>

    <div class="card status-card" :class="{ off: !state.toolsEnabled }">
      <div class="status-line">
        <Icon :name="state.toolsEnabled ? 'check' : 'alert'" :size="18" />
        <strong v-if="state.toolsEnabled">
          文件工具已启用（{{ usable }} 个目录可用）
        </strong>
        <strong v-else>文件工具未启用</strong>
      </div>
      <p class="muted tiny">
        <template v-if="state.toolsEnabled">
          Agent 现在可以调用
          <span v-for="(t, i) in state.tools" :key="t"><code class="mono">{{ t }}</code><span v-if="i < state.tools.length - 1">、</span></span>
          读取下面这些目录里的代码。路径越界在解析完符号链接之后判断，仓库里 checkin 的软链跑不出去。
        </template>
        <template v-else>
          没有配置任何可用的代码目录，所以
          <span v-for="(t, i) in state.tools" :key="t"><code class="mono">{{ t }}</code><span v-if="i < state.tools.length - 1">、</span></span>
          这三个工具<strong>根本不会注册</strong>——Agent 没有任何文件访问能力。这是诊断类 Agent 的合理默认。
        </template>
      </p>
    </div>

    <div class="card">
      <h2 class="card-title">代码目录</h2>
      <p class="muted tiny hint">
        每个目录是一棵可以被分析的代码树，通常一个目录下放多个项目。改动保存即热重载，无需重启。
        这些目录同时会对沙箱脚本可读，不用在「脚本沙箱设置」里再配一遍。
      </p>

      <p v-if="!draft.length" class="empty">还没有配置代码目录。</p>
      <ul v-else class="roots">
        <li v-for="(p, i) in draft" :key="p" class="root">
          <div class="root-head">
            <code class="mono path">{{ p }}</code>
            <span v-if="!statusOf(p)" class="badge">未保存</span>
            <span v-else-if="statusOf(p).exists" class="badge ok-badge">
              {{ statusOf(p).projects?.length || 0 }} 个项目{{ statusOf(p).more ? '+' : '' }}
            </span>
            <span v-else class="badge err-badge">{{ statusOf(p).error || '不可用' }}</span>
            <button class="btn btn-icon" title="移除" @click="removePath(i)">
              <Icon name="trash" :size="15" />
            </button>
          </div>
          <p v-if="statusOf(p)?.resolved" class="muted tiny">
            实际指向 <code class="mono">{{ statusOf(p).resolved }}</code>（软链）
          </p>
          <div v-if="statusOf(p)?.projects?.length" class="projects">
            <span v-for="name in statusOf(p).projects" :key="name" class="badge mono">{{ name }}</span>
            <span v-if="statusOf(p).more" class="muted tiny">…还有更多</span>
          </div>
        </li>
      </ul>

      <div class="add-row">
        <input
          v-model="newPath"
          class="input mono"
          placeholder="/data/repos（必须是绝对路径）"
          @keyup.enter="addPath"
        />
        <button class="btn" @click="addPath"><Icon name="plus" :size="15" /> 添加</button>
      </div>

      <p class="muted tiny warn-note">
        ⚠️ <strong>别把 agent 自己的配置/状态目录加进来</strong>——那里面是 API key 和会话库。
        越界检查能挡住仓库里的软链，但挡不住根目录本身就选错了；服务端会拒绝这一种情况。
      </p>

      <div class="form-actions">
        <button class="btn" v-if="dirty" @click="revert">撤销</button>
        <button class="btn btn-primary" :disabled="saving || !dirty" @click="save">
          {{ saving ? '保存中…' : dirty ? '保存并热重载' : '没有改动' }}
        </button>
      </div>
    </div>

    <div class="card">
      <h2 class="card-title">分析时的默认行为</h2>
      <ul class="facts">
        <li><code class="mono">grep_files</code> 定位，<code class="mono">read_file</code> 读上下文——先搜后读，别指望把整库喂进上下文。</li>
        <li>下列目录不扫：<span v-for="d in state.skipDirs" :key="d" class="badge mono">{{ d }}</span></li>
        <li>二进制文件不读（首 8KB 含 NUL 即跳过）；单次读 400 行 / 256KB 封顶；grep 默认 100 条匹配封顶。</li>
        <li>模型拿到的路径都是相对代码目录根的，它读回来的路径可以直接再传回去。</li>
      </ul>
      </div>
      </details>
    </div>
  </div>
</template>

<style scoped>
.view { display: flex; flex-direction: column; height: 100%; min-width: 0; }
.topbar { display: flex; align-items: center; justify-content: space-between; gap: var(--sp-3); padding: var(--sp-4) var(--sp-5); border-bottom: 1px solid var(--border); }
.topbar-l { display: flex; align-items: baseline; flex-wrap: wrap; gap: var(--sp-3); }
.topbar-l h1 { font-size: 18px; }
.sub { font-size: 12px; }
.body { flex: 1; overflow-y: auto; padding: var(--sp-5); width: 100%; max-width: 1200px; margin: 0 auto; }
.legacy-roots > .card { padding: 20px; margin-top: 12px; }
@media (max-width: 640px) { .body, .topbar { padding: 16px; } }

.legacy-roots summary { cursor: pointer; padding: 16px 0; color: var(--text-dim); font-size: 13px; }

.status-card {
  border-left: 3px solid #1a7f37;
}
.status-card.off {
  border-left-color: #b26a00;
}
.status-line {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.card-title {
  margin: 0 0 4px;
  font-size: 15px;
}
.hint {
  margin-top: 0;
}
.roots {
  list-style: none;
  margin: 12px 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.root {
  border: 1px solid var(--border, #e5e5e5);
  border-radius: 8px;
  padding: 10px 12px;
}
.root-head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.path {
  flex: 1;
  min-width: 200px;
  word-break: break-all;
}
.projects {
  margin-top: 8px;
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.ok-badge {
  background: #e8f5ec;
  color: #1a7f37;
}
.err-badge {
  background: #fdf0e3;
  color: #b26a00;
}
.add-row {
  display: flex;
  gap: 8px;
  align-items: center;
}
.add-row .input {
  flex: 1;
}
.empty {
  color: var(--muted, #888);
  font-size: 13px;
  margin: 12px 0;
}
.warn-note {
  margin-top: 12px;
}
.facts {
  margin: 8px 0 0;
  padding-left: 18px;
  font-size: 13px;
  line-height: 1.9;
  color: var(--muted, #666);
}
.facts .badge {
  margin-right: 4px;
}
</style>
