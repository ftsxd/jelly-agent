<script setup>
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import Icon from './components/Icon.vue'
import { navItems } from './router'
import { api } from './api'

const route = useRoute()
const health = ref(null)
const online = ref(false)
const authLoading = ref(true)
const authenticated = ref(false)
const mustChange = ref(false)
const configured = ref(true)
const username = ref('')
const password = ref('')
const loginError = ref('')
const newPassword = ref('')
const confirmPassword = ref('')

onMounted(async () => {
  try {
    health.value = await api.health()
    online.value = true
  } catch {
    online.value = false
  }
  try {
    const status = await api.authStatus()
    configured.value = status.configured !== false
    authenticated.value = status.authenticated === true
    mustChange.value = status.must_change === true
  } catch (err) {
    configured.value = false
    loginError.value = err.message
  } finally {
    authLoading.value = false
  }
})

async function login() {
  loginError.value = ''
  try {
    const result = await api.login(username.value, password.value)
    authenticated.value = true
    mustChange.value = result.must_change === true
    if (!mustChange.value) password.value = ''
  } catch (err) {
    loginError.value = err.message
  }
}

async function changePassword() {
  loginError.value = ''
  if (newPassword.value !== confirmPassword.value) {
    loginError.value = '两次输入的新密码不一致'
    return
  }
  try {
    await api.changePassword(password.value, newPassword.value)
    password.value = ''
    newPassword.value = ''
    confirmPassword.value = ''
    mustChange.value = false
  } catch (err) {
    loginError.value = err.message
  }
}

async function logout() {
  await api.logout()
  authenticated.value = false
  mustChange.value = false
}
</script>

<template>
  <div v-if="authLoading" class="auth-page"><span class="dim">正在检查管理员会话…</span></div>

  <div v-else-if="!configured" class="auth-page">
    <section class="auth-card">
      <h1>管理员尚未配置</h1>
      <p>请在终端执行 <code>jelly admin set-password &lt;用户名&gt;</code> 后重启控制台。</p>
    </section>
  </div>

  <div v-else-if="!authenticated" class="auth-page">
    <form class="auth-card" @submit.prevent="login">
      <h1>jelly-agent 控制台</h1>
      <p class="dim">请使用管理员账户登录</p>
      <label>用户名<input v-model="username" class="input" autocomplete="username" required /></label>
      <label>密码<input v-model="password" class="input" type="password" autocomplete="current-password" required /></label>
      <p v-if="loginError" class="auth-error">{{ loginError }}</p>
      <button class="btn primary" type="submit">登录</button>
    </form>
  </div>

  <div v-else-if="mustChange" class="auth-page">
    <form class="auth-card" @submit.prevent="changePassword">
      <h1>请修改初始密码</h1>
      <p class="dim">首次登录必须设置新的管理员密码。</p>
      <label>当前密码<input v-model="password" class="input" type="password" autocomplete="current-password" required /></label>
      <label>新密码<input v-model="newPassword" class="input" type="password" autocomplete="new-password" minlength="12" required /></label>
      <label>确认新密码<input v-model="confirmPassword" class="input" type="password" autocomplete="new-password" minlength="12" required /></label>
      <p v-if="loginError" class="auth-error">{{ loginError }}</p>
      <button class="btn primary" type="submit">保存新密码</button>
    </form>
  </div>

  <div v-else class="shell">
    <aside class="sidebar">
      <div class="brand">
        <svg class="brand-mark" width="28" height="28" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <path
            d="M4 11a8 8 0 0 1 16 0v1H4z"
            fill="var(--primary)"
            fill-opacity="0.9"
          />
          <path
            d="M6.5 12.5c0 2 .8 3 .8 4.5M10 12.5c0 2.5-.6 3.5-.6 5.5M14 12.5c0 2.5.6 3.5.6 5.5M17.5 12.5c0 1.5-.8 2.5-.8 4.5"
            stroke="var(--accent)"
            stroke-width="1.4"
            stroke-linecap="round"
          />
        </svg>
        <div class="brand-text">
          <strong>jelly-agent</strong>
          <span class="mono dim brand-sub">控制台</span>
        </div>
      </div>

      <nav class="nav" aria-label="主导航">
        <RouterLink
          v-for="item in navItems"
          :key="item.path"
          :to="item.path"
          class="nav-item"
          :class="{ active: route.name === item.name }"
        >
          <Icon :name="item.meta.icon" :size="18" />
          <span>{{ item.meta.title }}</span>
        </RouterLink>
      </nav>

      <div class="sidebar-foot">
        <div class="status">
          <span class="dot" :class="{ ok: online }" />
          <span class="mono">{{ online ? '已连接' : '离线' }}</span>
        </div>
        <span v-if="health" class="mono dim ver">v{{ health.version }}</span>
        <button class="logout" title="退出管理员登录" @click="logout">退出</button>
      </div>
    </aside>

    <main class="content">
      <RouterView v-slot="{ Component }">
        <component :is="Component" />
      </RouterView>
    </main>
  </div>
</template>

<style scoped>
.shell {
  display: grid;
  grid-template-columns: var(--nav-w) 1fr;
  height: 100%;
}

.sidebar {
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border-right: 1px solid var(--border);
  padding: var(--sp-4) var(--sp-3);
}

.brand {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  padding: var(--sp-2) var(--sp-2) var(--sp-5);
}
.brand-mark {
  flex-shrink: 0;
  filter: drop-shadow(0 0 10px var(--primary-border));
}
.brand-text {
  display: flex;
  flex-direction: column;
  line-height: 1.2;
}
.brand-text strong {
  font-size: 15px;
  letter-spacing: -0.01em;
}
.brand-sub {
  font-size: 11px;
}

.nav {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.nav-item {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--radius-sm);
  color: var(--text-dim);
  font-weight: 500;
  transition: background 0.15s ease, color 0.15s ease;
}
.nav-item:hover {
  background: var(--surface-2);
  color: var(--text);
}
.nav-item.active {
  background: var(--primary-tint);
  color: var(--primary);
}

.sidebar-foot {
  margin-top: auto;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--sp-3) var(--sp-2) var(--sp-1);
  border-top: 1px solid var(--border);
  font-size: 12px;
}
.status {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  color: var(--text-dim);
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--text-muted);
}
.dot.ok {
  background: var(--accent);
  box-shadow: 0 0 8px var(--accent);
}
.ver {
  font-size: 11px;
}

.content {
  overflow: hidden;
  height: 100%;
}
.auth-page {
  display: grid;
  min-height: 100%;
  place-items: center;
  padding: var(--sp-5);
}
.auth-card {
  display: grid;
  width: min(100%, 360px);
  gap: var(--sp-3);
  padding: var(--sp-6);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--surface);
}
.auth-card h1 { margin: 0; font-size: 20px; }
.auth-card p { margin: 0; line-height: 1.6; }
.auth-card label { display: grid; gap: var(--sp-2); font-size: 13px; color: var(--text-dim); }
.auth-error { color: var(--danger); font-size: 13px; }
.logout { margin-left: var(--sp-2); border: 0; background: transparent; color: var(--text-muted); cursor: pointer; font-size: 12px; }
.logout:hover { color: var(--text); }

@media (max-width: 720px) {
  .shell {
    grid-template-columns: 1fr;
    grid-template-rows: auto 1fr;
  }
  .sidebar {
    flex-direction: row;
    align-items: center;
    border-right: none;
    border-bottom: 1px solid var(--border);
    padding: var(--sp-2) var(--sp-3);
  }
  .brand {
    padding: 0;
  }
  .nav {
    flex-direction: row;
    margin-left: var(--sp-4);
  }
  .nav-item span {
    display: none;
  }
  .sidebar-foot {
    margin: 0 0 0 auto;
    border: none;
    padding: 0;
  }
}
</style>
