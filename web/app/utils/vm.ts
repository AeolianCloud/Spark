/**
 * VM 领域展示工具：状态徽章映射、生命周期操作可用性与容量/时长格式化。
 * 列表页与详情页共用，保证两页展示与判定一致。
 */
import { vmStatusLabel } from './format'
import type { VMListItem } from '~/api'

/** 徽章展示元数据：文案 + 颜色（未知 PVE 穿透状态给中性样式） */
export interface VMStatusBadge {
  /** 徽章颜色 */
  color: 'success' | 'warning' | 'error' | 'info' | 'neutral'
  /** 状态中文标签 */
  label: string
}

/** 已知状态 → 徽章颜色；其余为 PVE 穿透未知状态（paused 等），统一中性样式 */
const STATUS_COLORS: Record<string, VMStatusBadge['color']> = {
  running: 'success',
  stopped: 'neutral',
  paused: 'warning',
  suspended: 'warning',
  creating: 'info',
  failed: 'error'
}

/** 状态徽章映射：label 复用 vmStatusLabel，未知状态中性样式；
 *  hasOwn 防御原型链键（契约 status 为自由字符串），?? 收窄 noUncheckedIndexedAccess 下的 undefined */
export function vmStatusBadge(status: string): VMStatusBadge {
  const color = Object.hasOwn(STATUS_COLORS, status) ? STATUS_COLORS[status] : undefined
  return { color: color ?? 'neutral', label: vmStatusLabel(status) }
}

// ---- 本地过渡状态（生命周期操作在途，design D2/D3）----
// 操作受理后由 useVMPendingAction 标记 pending_* 状态，观察轮询期间展示
// 「启动中/关闭中/重启中」info 徽章；列表页与详情页共用，与 vmStatusBadge 同风格。

/** 生命周期操作类型（与 VmActions 的 pendingAction prop 一致） */
export type PendingAction = 'start' | 'stop' | 'restart'

/** 操作中文标签：受理/结果 toast 与 pending 徽章共用（如「正在启动」「已启动」） */
export const PENDING_ACTION_LABELS: Record<PendingAction, string> = {
  start: '启动',
  stop: '关闭',
  restart: '重启'
}

/** 过渡状态徽章：一律 info 色，文案为「动作 + 中」 */
export function vmPendingStatusBadge(action: PendingAction): VMStatusBadge {
  return { color: 'info', label: `${PENDING_ACTION_LABELS[action]}中` }
}

/** 本地过渡状态：供给进行中（creating），PVE 对端可能正在创建实体，操作有竞态 */
export function isProvisioningStatus(status: string): boolean {
  return status === 'creating'
}

/** 供给失败终态（failed）：无（或只有残留半成品）PVE 实体，允许清理销毁 */
export function isFailedStatus(status: string): boolean {
  return status === 'failed'
}

/** 来源徽章展示元数据：文案 + 颜色（列表页 source 列三态） */
export interface VMSourceBadge {
  /** 徽章颜色 */
  color: 'success' | 'warning' | 'info' | 'neutral'
  /** 来源中文标签 */
  label: string
}

/** 来源徽章映射：spark_created=Spark 创建、claimed=已认领、external=外部 VM（未纳管，需认领） */
export function vmSourceBadge(source: VMListItem['source']): VMSourceBadge {
  switch (source) {
    case 'spark_created':
      return { color: 'info', label: 'Spark 创建' }
    case 'claimed':
      return { color: 'success', label: '已认领' }
    case 'external':
      return { color: 'warning', label: '外部 VM' }
    default:
      return { color: 'neutral', label: '未知来源' }
  }
}

// ---- 生命周期操作可用性 ----
// creating/failed 禁用启停/重启/调规格（后端 vm_not_ready 兜底）；
// failed 可销毁（清理本地记录、释放 IP、purge 残留半成品）；
// 其余状态按运行态判定（未知状态从宽放行），后端另有 vm_not_ready（409）兜底。

/** 基本可操作性：排除供给中与供给失败，仅放行可运行态（启动/停止/重启/调整规格） */
export function canOperateVM(status: string): boolean {
  return !isProvisioningStatus(status) && !isFailedStatus(status)
}

/** 可启动：非运行中即可（含 paused 与未知状态） */
export function canStartVM(status: string): boolean {
  return canOperateVM(status) && status !== 'running'
}

/** 可关闭：非已停止即可（含 paused 与未知状态） */
export function canStopVM(status: string): boolean {
  return canOperateVM(status) && status !== 'stopped'
}

/** 可重启：仅运行中或未知状态（已停止/已暂停时重启无意义）；hasOwn 防御原型链键 */
export function canRestartVM(status: string): boolean {
  return canOperateVM(status) && (status === 'running' || !Object.hasOwn(STATUS_COLORS, status))
}

/** 可调整规格：非供给中/供给失败即可（基于 canOperateVM；磁盘缩小由前端预检 + 后端 422 兜底） */
export function canResizeVM(status: string): boolean {
  return canOperateVM(status)
}

/** 可销毁：可运行态或供给失败均可（failed 用于清理本地记录、释放 IP、purge 残留半成品；销毁必须二次确认） */
export function canDestroyVM(status: string): boolean {
  return canOperateVM(status) || isFailedStatus(status)
}

/** 字节 → 可读容量（自动选择 KB/MB/GB/TB，保留 1 位小数；缺省显示占位） */
export function formatBytes(bytes?: number): string {
  if (bytes === undefined || bytes === null) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i += 1
  }
  return i === 0 ? `${value} ${units[i]}` : `${value.toFixed(1)} ${units[i]}`
}

/** 内存 MB → 可读容量（≥1GB 显示 GB，否则 MB）；缺省显示占位 */
export function formatMemMB(memMb?: number): string {
  if (memMb === undefined || memMb === null) return '—'
  if (memMb >= 1024 && memMb % 1024 === 0) return `${memMb / 1024} GB`
  if (memMb >= 1024) return `${(memMb / 1024).toFixed(1)} GB`
  return `${memMb} MB`
}

/** 秒 → 可读时长（天/小时/分钟）；缺省显示占位 */
export function formatUptime(seconds?: number): string {
  if (seconds === undefined || seconds === null) return '—'
  if (seconds < 60) return `${seconds} 秒`
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (hours === 0) return `${minutes} 分钟`
  if (hours < 24) return `${hours} 小时 ${minutes} 分钟`
  const days = Math.floor(hours / 24)
  return `${days} 天 ${hours % 24} 小时`
}
