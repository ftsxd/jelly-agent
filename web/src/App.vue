<script setup>
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import Icon from './components/Icon.vue'
import { navItems } from './router'
import { api } from './api'

const route = useRoute()
const health = ref(null)
const online = ref(false)

onMounted(async () => {
  try {
    health.value = await api.health()
    online.value = true
  } catch {
    online.value = false
  }
})
</script>

<template>
  <div class="shell">
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
