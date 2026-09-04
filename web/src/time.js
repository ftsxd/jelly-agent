// Relative timestamps for lists.
//
// A session list is scanned, not read: "2026/9/3 22:24:43" forces the reader to
// diff two 19-character strings in their head to work out which run is the one
// they just did. "12 分钟前" answers that without arithmetic. The absolute time
// stays available on hover, because once you have found the session you often
// do want the exact moment.

const MIN = 60
const HOUR = 60 * MIN
const DAY = 24 * HOUR

/** Human-scannable age, e.g. 刚刚 / 12 分钟前 / 昨天 21:04 / 9月3日. */
export function relTime(unix) {
  if (!unix) return ''
  const now = Date.now() / 1000
  const d = Math.max(0, now - unix) // a clock skew into the future reads as 刚刚
  if (d < MIN) return '刚刚'
  if (d < HOUR) return `${Math.floor(d / MIN)} 分钟前`
  if (d < DAY) return `${Math.floor(d / HOUR)} 小时前`

  const then = new Date(unix * 1000)
  const today = new Date()
  const midnight = new Date(today.getFullYear(), today.getMonth(), today.getDate()).getTime() / 1000
  if (unix >= midnight - DAY) return `昨天 ${pad(then.getHours())}:${pad(then.getMinutes())}`
  if (then.getFullYear() === today.getFullYear()) return `${then.getMonth() + 1}月${then.getDate()}日`
  return `${then.getFullYear()}/${then.getMonth() + 1}/${then.getDate()}`
}

/** Full timestamp, for the title attribute behind a relative one. */
export function absTime(unix) {
  if (!unix) return ''
  return new Date(unix * 1000).toLocaleString('zh-CN', { hour12: false })
}

function pad(n) {
  return String(n).padStart(2, '0')
}
