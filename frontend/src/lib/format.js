const int = new Intl.NumberFormat('tr-TR')
const one = new Intl.NumberFormat('tr-TR', { minimumFractionDigits: 1, maximumFractionDigits: 1 })

export const fmtInt = (n) => int.format(Math.round(Number(n) || 0))
export const fmtRate = (n) => one.format(Number(n) || 0)
export function fmtPercent(part, total) {
  if (!total) return '0'
  return int.format(Math.floor((part / total) * 100))
}
export function fmtDuration(seconds) {
  const s = Math.max(0, Math.round(Number(seconds) || 0))
  if (s < 60) return `${s} sn`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} dk ${s % 60} sn`
  return `${Math.floor(m / 60)} sa ${m % 60} dk`
}
