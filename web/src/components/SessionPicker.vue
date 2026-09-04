<!--
  Picking a past session out of a native <select>.

  A native option is one line of unstyled text, so the old control had to flatten
  "what the session was about" and "which session it is" into one string —
  "用 fetch_url 抓 … · cli-1788445482089039000" — and then the OS drew it in its
  own dark menu, which the light theme cannot reach. Neither half was readable:
  the part you search by was truncated by the control's width, and the part you
  never search by (a 19-digit epoch-nanos id) took most of the room.

  So: title on its own line, id and age demoted to a caption, and a filter box,
  since a picker you cannot type into stops working at about thirty sessions.
-->
<script setup>
import { computed, nextTick, ref, watch } from 'vue'
import Icon from './Icon.vue'
import { absTime, relTime } from '../time'

const props = defineProps({
  sessions: { type: Array, default: () => [] },
  modelValue: { type: String, default: '' },
  disabled: { type: Boolean, default: false },
})
const emit = defineEmits(['update:modelValue', 'pick'])

const open = ref(false)
const q = ref('')
const root = ref(null)
const search = ref(null)

const current = computed(() => props.sessions.find((s) => s.id === props.modelValue) || null)

const matches = computed(() => {
  const needle = q.value.trim().toLowerCase()
  if (!needle) return props.sessions
  // Both fields: you might remember the topic, or be pasting an id from a log.
  return props.sessions.filter(
    (s) => (s.preview || '').toLowerCase().includes(needle) || s.id.toLowerCase().includes(needle),
  )
})

watch(open, async (isOpen) => {
  if (!isOpen) {
    q.value = ''
    document.removeEventListener('mousedown', onOutside)
    return
  }
  document.addEventListener('mousedown', onOutside)
  await nextTick()
  search.value?.focus()
})

function onOutside(e) {
  if (!root.value?.contains(e.target)) open.value = false
}

function pick(s) {
  open.value = false
  emit('update:modelValue', s.id)
  emit('pick', s.id)
}
</script>

<template>
  <div class="picker" ref="root">
    <button
      class="input picker-trigger"
      type="button"
      :disabled="disabled"
      :aria-expanded="open"
      @click="open = !open"
    >
      <span class="picker-label" :class="{ placeholder: !current }">
        {{ current ? current.preview || '（空会话）' : '历史会话…' }}
      </span>
      <Icon name="chevron" :size="14" class="picker-caret" />
    </button>

    <div v-if="open" class="picker-panel">
      <div class="picker-search">
        <Icon name="search" :size="14" />
        <input ref="search" v-model="q" placeholder="搜索标题或会话 ID" @keydown.esc="open = false" />
      </div>

      <div v-if="!matches.length" class="picker-empty">
        {{ sessions.length ? '没有匹配的会话' : '还没有历史会话' }}
      </div>

      <ul v-else class="picker-list">
        <li v-for="s in matches" :key="s.id">
          <button type="button" class="picker-item" :class="{ on: s.id === modelValue }" @click="pick(s)">
            <span class="picker-title">{{ s.preview || '（空会话）' }}</span>
            <span class="picker-meta">
              <span class="mono">{{ s.id }}</span>
              <span :title="absTime(s.last_update)">{{ relTime(s.last_update) }}</span>
            </span>
          </button>
        </li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
.picker {
  position: relative;
  min-width: 0;
}
.picker-trigger {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  width: 100%;
  max-width: 260px;
  cursor: pointer;
  text-align: left;
}
.picker-label {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.picker-label.placeholder {
  color: var(--text-muted);
}
.picker-caret {
  flex-shrink: 0;
  color: var(--text-muted);
}

.picker-panel {
  position: absolute;
  z-index: 50;
  top: calc(100% + 6px);
  right: 0;
  width: min(420px, 80vw);
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  box-shadow: var(--shadow-pop);
  overflow: hidden;
}
.picker-search {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-3);
  border-bottom: 1px solid var(--hairline);
  color: var(--text-muted);
}
.picker-search input {
  flex: 1;
  min-width: 0;
  border: 0;
  background: none;
  outline: none;
  font: inherit;
  color: var(--text);
}
.picker-search input::placeholder {
  color: var(--text-muted);
}

.picker-list {
  margin: 0;
  padding: var(--sp-1);
  list-style: none;
  max-height: 320px;
  overflow-y: auto;
}
.picker-item {
  display: grid;
  gap: 2px;
  width: 100%;
  padding: var(--sp-2) var(--sp-3);
  border: 0;
  border-radius: var(--radius-sm);
  background: none;
  text-align: left;
  cursor: pointer;
  font: inherit;
  color: var(--text);
}
.picker-item:hover {
  background: var(--surface-2);
}
.picker-item.on {
  background: var(--primary-tint);
}
/* The title is what you scan; give it the whole line and let it wrap once
   rather than truncating a sentence to the width of the control. */
.picker-title {
  font-weight: 500;
  line-height: 1.4;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.picker-meta {
  display: flex;
  gap: var(--sp-2);
  align-items: baseline;
  font-size: 11px;
  color: var(--text-muted);
}
.picker-meta .mono {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.picker-empty {
  padding: var(--sp-5);
  text-align: center;
  color: var(--text-muted);
  font-size: 13px;
}
</style>
