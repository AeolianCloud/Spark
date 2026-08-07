/**
 * 共享展示工具：时间格式化与状态标签映射。
 * 不引入第三方依赖，全部使用内置 Intl API。
 */

/** 契约 date-time 为 RFC3339 字符串；本地时区格式化展示，非法值原样返回 */
export function formatDateTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { hour12: false })
}

/** 0-1 小数使用率 → 百分比字符串（如 0.153 → "15%"）；非法值/缺省显示占位 */
export function formatPercent(usage?: number | null): string {
  if (usage === undefined || usage === null || !Number.isFinite(usage)) return '—'
  const n = Math.round(Math.min(1, Math.max(0, usage)) * 100)
  return `${n}%`
}

/**
 * VM 状态中文标签（契约 status 为自由字符串，未知状态统一归入"其他"）。
 * creating/failed 为本地过渡状态；ready 及以后为 PVE 穿透状态（running/stopped 等）。
 */
export function vmStatusLabel(status: string): string {
  const labels: Record<string, string> = {
    running: '运行中',
    stopped: '已停止',
    paused: '已暂停',
    suspended: '已挂起',
    creating: '创建中',
    failed: '失败'
  }
  // hasOwn 防御原型链键（契约 status 为自由字符串，如 status="toString" 时不得命中 Object.prototype），
  // ?? 收窄 noUncheckedIndexedAccess 下的 undefined
  const label = Object.hasOwn(labels, status) ? labels[status] : undefined
  return label ?? '其他'
}

/**
 * UForm 无 schema 场景下自定义校验函数的返回类型（与 UForm validate 返回结构一致），
 * 避免页面重复声明。
 */
export interface FormValidateError {
  /** 对应 UFormField 的 name，用于把错误挂到具体字段 */
  name?: string
  message: string
}
