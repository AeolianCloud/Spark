/**
 * VM 领域展示工具：状态徽章映射、生命周期操作可用性与容量/时长格式化。
 * 列表页与详情页共用，保证两页展示与判定一致。
 */
import { vmStatusLabel } from './format'

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

/** 本地过渡状态：供给未完成（creating）或供给失败（failed），PVE 对端尚不可操作 */
export function isProvisioningStatus(status: string): boolean {
  return status === 'creating' || status === 'failed'
}

// ---- 生命周期操作可用性 ----
// creating/failed 为本地过渡状态，禁用启停/调整规格/销毁；
// 其余状态按运行态判定（未知状态从宽放行），后端另有 vm_not_ready（409）兜底。

/** 基本可操作性：非过渡状态均可尝试（启动/停止/重启/调整规格/销毁） */
export function canOperateVM(status: string): boolean {
  return !isProvisioningStatus(status)
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

/** 可调整规格：非过渡状态即可（磁盘缩小由前端预检 + 后端 422 兜底） */
export function canResizeVM(status: string): boolean {
  return canOperateVM(status)
}

/** 可销毁：非过渡状态即可（销毁必须二次确认） */
export function canDestroyVM(status: string): boolean {
  return canOperateVM(status)
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
