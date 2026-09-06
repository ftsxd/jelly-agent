/**
 * Task-centre logic, kept out of the component.
 *
 * There is no component test harness in this project — no @vue/test-utils, no
 * happy-dom — so anything that lives inside a .vue file is untested by
 * construction. timeline.js and latest.js are both here for that reason, and
 * both earned it: a race guard that lived in ChatView.vue was wrong for two
 * releases because nothing could exercise it.
 *
 * Everything below is a pure function over the DTOs the server sends.
 */

/** Status vocabulary, matching internal/server/task.go. */
export const STATUS = {
  running: { label: '进行中', icon: 'spark', tone: 'run' },
  completed: { label: '已完成', icon: 'check', tone: 'ok' },
  failed: { label: '失败', icon: 'alert', tone: 'bad' },
  cancelled: { label: '已取消', icon: 'power', tone: 'muted' },
  // Declared but never produced today: nothing can say a step is waiting for a
  // person, because the approval machinery is not wired. They are here so that
  // the day the backend can say it, the page already renders it — and so that
  // an unknown status never falls through to a blank cell.
  pending: { label: '等待执行', icon: 'chevron', tone: 'muted' },
  waiting_input: { label: '等待输入', icon: 'user', tone: 'warn' },
  blocked: { label: '已阻塞', icon: 'alert', tone: 'warn' },
}

const UNKNOWN_STATUS = { label: '未知', icon: 'doc', tone: 'muted' }

/**
 * statusOf never returns undefined.
 *
 * A status the frontend has not heard of is a backend that learned something
 * new, not a reason to render an empty badge — the label falls back to the raw
 * value so it is at least legible and reportable.
 */
export function statusOf(status) {
  if (!status) return UNKNOWN_STATUS
  return STATUS[status] || { ...UNKNOWN_STATUS, label: String(status) }
}

/** Task types, matching internal/server/task.go. */
export const TYPES = {
  monitor: '监控查询',
  log: '日志分析',
  inspection: '日常巡检',
  other: '其他任务',
}

export function typeLabel(type) {
  return TYPES[type] || TYPES.other
}

/** True when a task is still moving, so the page knows whether to poll. */
export function isLive(task) {
  return task?.status === 'running'
}

/**
 * needsAttention marks a task the user has to do something about.
 *
 * Deliberately not "anything that is not completed": a running task needs
 * nothing from anyone, and marking it would make the signal useless on a busy
 * page.
 */
export function needsAttention(task) {
  return task?.status === 'waiting_input' || task?.status === 'blocked' || task?.status === 'failed'
}

/** Whether any task in the list is still running. */
export function anyLive(tasks) {
  return (tasks || []).some(isLive)
}

/**
 * Per-task selection, so switching away and back returns to where you were.
 *
 * Returns plain values and takes plain values. It deliberately holds no
 * reactive state: the first version kept the current selection in a Map and
 * had the component read it through a computed, which never re-ran because a
 * plain Map is not reactive — so clicking a step did nothing at all. The
 * component owns the refs; this owns only the memory of what each task had
 * selected.
 */
export function selectionStore() {
  const byTask = new Map()
  return {
    get(taskID) {
      return byTask.get(taskID) || { step: '', artifact: '' }
    },
    set(taskID, sel) {
      byTask.set(taskID, { step: sel.step || '', artifact: sel.artifact || '' })
    },
    forget(taskID) {
      byTask.delete(taskID)
    },
  }
}

/**
 * Which step a click should select, given what is selected now.
 *
 * Clicking the open step closes it; clicking another opens that one. Pulled
 * out because it is the kind of two-line rule that silently inverts during a
 * refactor and has no test if it lives in a template.
 */
export function toggleStep(current, stepID) {
  return current === stepID ? '' : stepID
}

/**
 * artifactsOfStep answers "what did this step produce".
 *
 * The link is the step id the server put on each artifact, not a guess from
 * timestamps — two steps can produce artifacts in the same millisecond.
 */
export function artifactsOfStep(artifacts, stepID) {
  if (!stepID) return []
  return (artifacts || []).filter((a) => a.step === stepID)
}

/** stepOfArtifact is the reverse: click a product, find where it came from. */
export function stepOfArtifact(steps, artifact) {
  if (!artifact?.step) return null
  return (steps || []).find((s) => s.id === artifact.step) || null
}

/**
 * artifactState describes what can be done with one product.
 *
 * Three separate facts that are routinely mistaken for one another: complete
 * says the model got all of it, retrievable says the bytes are still in the
 * store, expired says retention dropped them. A truncated-but-retrievable
 * result is normal; an expired one is not, and must not render as an empty
 * preview.
 *
 * retrievable is answered by the store, not by what the model was told at the
 * time. Deriving it from the old event marked every result recorded before
 * that flag existed as "未落库" while its bytes sat in the database, readable.
 */
export function artifactState(a) {
  if (!a) return { readable: false, note: '' }
  if (a.expired) {
    return { readable: false, tone: 'bad', badge: '已过期', note: '结果已过期，请重新查询' }
  }
  if (!a.retrievable) {
    return {
      readable: false, tone: 'warn', badge: '未保存',
      note: '这次返回没有留在结果存储里，只能看到调用当时记录的摘要',
    }
  }
  if (!a.complete) {
    return { readable: true, tone: 'warn', badge: '部分进入上下文', note: '完整内容已保存，可搜索或分段读取' }
  }
  return { readable: true, tone: 'ok', badge: '完整', note: '' }
}

/**
 * emptyReason says precisely why there is nothing to show.
 *
 * "暂无产物" for a task that produced none and "还没跑到" for one that is still
 * running are different facts, and a single empty state that covers both tells
 * the reader nothing.
 */
export function emptyReason(task, artifacts) {
  if (!task) return '选择左侧任务查看执行过程'
  if ((artifacts || []).length > 0) return ''
  if (isLive(task)) return '任务进行中，还没有产物'
  if (task.status === 'failed') return '任务失败，没有产生产物'
  // Not "nothing happened": every tool result is kept, but only the ones worth
  // opening on their own are listed here. The small ones are in their step.
  return '这个任务的结果都很小，直接在步骤详情里查看'
}

/**
 * stepSummary is the one-line account of a step for the collapsed row.
 *
 * Prefers the model's own narration, because it said what it was doing at the
 * time. Falls back to counting the calls rather than inventing a description
 * of them.
 */
export function stepSummary(step) {
  if (!step) return ''
  if (step.note) return step.note
  if (step.error) return step.error
  if (step.calls) return `${step.calls} 次工具调用`
  return ''
}

/** Human byte size, mirroring format.js so the two never disagree. */
export { fmtBytes } from './format'
