/**
 * VM 生命周期操作执行反馈组合函数（design D1/D2/D3/D5）：
 * - 操作受理后启动 3s 观察定时器，对目标 VM 走 getVM 单查（本地行与 external 行统一，
 *   整页 15s 自动刷新逻辑不动），直至 PVE 状态确认生效
 * - 判定表（D2）：start → status==='running'；stop → status==='stopped'；
 *   restart → ① uptime 下降（D3：kvm 进程重建后从 0 重计时，零容差）
 *   ② 状态变迁 stopped→running 兜底（uptime 快照缺失时）
 * - 90s 超时兜底：按超时时刻状态区分提示（restart 且停机关 → error「重启中断」；
 *   running → warning「仍在执行」）
 * - 两段式 toast（D5）：受理 info「正在启动/关闭/重启…」→ 结果 success/error/warning；
 *   失败保留行内错误条（pendingError）
 * - 单实例同时只跟踪一个在途操作（busy-any 语义），列表页按行定位 pending 目标；
 *   成功/失败/超时即停定时器，组件卸载时清理
 * - 详情页路由切换（组件复用）时调用 stop() 终止在途观察：清 pending、递增序号
 *   丢弃在途响应，防止旧 VM 的观测结果回写新 VM 页面、操作误作用于旧 VM
 */
import type { VMListItem } from '~/api'
import { ApiError, getVM, restartVM, startVM, stopVM } from '~/api'
import { PENDING_ACTION_LABELS, type PendingAction } from '~/utils/vm'

/** 观察轮询间隔（3s，design D1） */
const POLL_INTERVAL_MS = 3_000
/** 操作超时兜底时长（90s，design D2） */
const TIMEOUT_MS = 90_000

/** 观测快照：判定 restart uptime 下降所需的最小状态集 */
interface VMStateSnapshot {
  status: string
  /** 运行时长（秒）；停机或 PVE 对端缺省时省略 */
  uptime?: number
}

/** 单个 VM 条目 → 观测快照（条目缺失返回 null） */
function vmStateOf(vm: VMListItem | null | undefined): VMStateSnapshot | null {
  if (!vm) return null
  return { status: vm.status, uptime: vm.uptime }
}

/** 在途操作（含目标 id） */
export interface VMPendingTarget {
  /** 目标 VM 标识（数字本地行 id 或 ext- 合成标识） */
  id: string | number
  /** 在途操作类型 */
  action: PendingAction
}

export interface UseVMPendingActionOptions {
  /** 读取目标 VM 最新条目（判定基准与受理 toast 名称；行缺失/详情未加载时返回 null） */
  getVMState: (id: string | number) => VMListItem | null
  /** 观测到新状态快照时回调（页面就地更新行/详情数据，无需等待整页刷新） */
  onSnapshot?: (vm: VMListItem) => void
  /** 受理后立即回调（页面刷新一次，让 PVE 状态尽快可见） */
  onAccepted?: (id: string | number) => void
}

export interface VMPendingActionState {
  /** 在途操作：null 表示无在途操作 */
  pending: Readonly<Ref<VMPendingTarget | null>>
  /** 行内错误条数据（受理失败 / 超时判定失败），AppErrorAlert 直接消费 */
  pendingError: Readonly<Ref<ApiError | null>>
  /** 触发生命周期操作：受理成功启动观察轮询并返回 true；失败（含已在途）返回 false */
  run: (action: PendingAction, id: string | number) => Promise<boolean>
  /** 终止在途观察并清空在途状态：详情页路由切换（组件复用）时调用，
   *  递增序号丢弃在途响应，防止旧 VM 观测结果回写新 VM 页面 */
  stop: () => void
  /** 清空行内错误条 */
  clearError: () => void
}

export function useVMPendingAction(options: UseVMPendingActionOptions): VMPendingActionState {
  const toast = useToast()

  const pending = ref<VMPendingTarget | null>(null)
  const pendingError = ref<ApiError | null>(null)

  // 组件已卸载标志：卸载后 run 的 await 返回不再继续 toast/onAccepted/startObserving
  let disposed = false
  // 观察轮询守卫：观察期序号（成功/失败/超时/卸载时递增，使在途轮询过期丢弃）
  let observeSeq = 0
  // 上一次观测快照（restart uptime 下降判定基准；受理瞬间以页面当前条目为初值）
  let lastObserved: VMStateSnapshot | null = null
  let timer: ReturnType<typeof setInterval> | null = null
  let timeoutTimer: ReturnType<typeof setTimeout> | null = null

  function clearError(): void {
    pendingError.value = null
  }

  /** 停止观察（成功/失败/超时/卸载）：递增序号丢弃在途轮询，清理定时器 */
  function stopObserving(): void {
    observeSeq++
    if (timer) {
      clearInterval(timer)
      timer = null
    }
    if (timeoutTimer) {
      clearTimeout(timeoutTimer)
      timeoutTimer = null
    }
  }

  /** 终止在途观察并清空在途状态（路由切换/组件卸载共用）：
   *  递增序号使在途响应过期，防止旧 VM 观测结果回写新 VM 页面 */
  function stop(): void {
    stopObserving()
    pending.value = null
    pendingError.value = null
  }

  /**
   * 完成判定（design D2/D3）：
   * - start：status → running
   * - stop：status → stopped
   * - restart：① uptime 下降（当前观测值 < 上一次观测值，零容差）
   *   ② 状态变迁 stopped→running（uptime 快照缺失时兜底）
   */
  function judgeCompletion(action: PendingAction, current: VMStateSnapshot | null, prev: VMStateSnapshot | null): boolean {
    if (!current) return false
    if (action === 'start') return current.status === 'running'
    if (action === 'stop') return current.status === 'stopped'
    // restart：uptime 为可选字段（停机时省略），双方均缺省时无法比较，交给状态变迁信号②
    if (prev?.uptime !== undefined && current.uptime !== undefined && current.uptime < prev.uptime) return true
    return prev?.status === 'stopped' && current.status === 'running'
  }

  /** 单次观察：getVM 单查目标 VM，快照回调页面并做完成判定 */
  async function pollOnce(action: PendingAction, id: string | number, seq: number): Promise<void> {
    try {
      const res = await getVM(id)
      // 过期响应丢弃：期间观察已停止或已重启新一轮
      if (seq !== observeSeq) return
      const vm = res.data
      // 目标校验（seq 守卫之外的防御双保险）：仅当该轮观察仍是当前在途目标时
      // 才回写页面（stop() 后残留轮询即使序号巧合命中也不得污染新页面数据）
      if (pending.value?.id !== id) return
      options.onSnapshot?.(vm)
      const observed = vmStateOf(vm)
      if (judgeCompletion(action, observed, lastObserved)) {
        stopObserving()
        pending.value = null
        toast.add({
          title: `已${PENDING_ACTION_LABELS[action]}`,
          description: `「${vm.name}」${PENDING_ACTION_LABELS[action]}完成`,
          color: 'success',
          icon: 'i-lucide-check-circle-2'
        })
        return
      }
      // 观测为空时保留旧基准（不覆盖 lastObserved）：瞬时 200 空体不会重置
      // restart 的 uptime 下降判定基准，否则该信号永久失效，stopped 窗口只能等 90s 超时
      if (observed) lastObserved = observed
    } catch {
      // 观测中断（节点抖动/VM 瞬时不可达等）：维持转圈继续等，超时兜底（design Risks）
    }
  }

  /** 90s 超时兜底：按超时时刻状态快照区分提示（design D2） */
  function handleTimeout(action: PendingAction, id: string | number, seq: number): void {
    if (seq !== observeSeq) return
    stopObserving()
    pending.value = null
    // 超时时刻状态：优先取最近观测值，回落页面当前条目（仍可能为 null：无任何观测成功）
    const status = (lastObserved ?? vmStateOf(options.getVMState(id)))?.status
    if (action === 'restart' && status === 'stopped') {
      // restart 超时且停机关：判定为中断（优雅关闭后未回到运行态），error + 行内错误条
      const err = new ApiError(0, 'unknown', '重启中断：90 秒内未观察到重启完成，VM 当前处于已停止状态')
      pendingError.value = err
      toast.add({ title: '重启中断', description: '90 秒内未观察到重启完成，VM 当前已停止', color: 'error', icon: 'i-lucide-circle-x' })
      return
    }
    // 其余（start/stop 超时，或 restart 超时仍在运行）：warning，不误导为失败
    const tips: Record<PendingAction, string> = {
      start: '启动较慢仍在执行，请稍后刷新查看最新状态',
      stop: '关闭较慢仍在执行，请稍后刷新查看最新状态',
      restart: '重启仍在执行，请稍后刷新查看最新状态'
    }
    toast.add({
      title: '仍在执行',
      description: tips[action],
      color: 'warning',
      icon: 'i-lucide-alert-triangle'
    })
  }

  /** 受理后启动观察：3s 轮询 + 90s 超时兜底 */
  function startObserving(action: PendingAction, id: string | number): void {
    // 防御性清理：理论上 run 守卫保证此处无在途观察，兜底清理残留定时器
    stopObserving()
    const seq = ++observeSeq
    // 初始观测基准：受理瞬间页面条目快照（restart 的 uptime 下降判定依赖该基准）
    lastObserved = vmStateOf(options.getVMState(id))
    timer = setInterval(() => {
      if (seq !== observeSeq) return
      void pollOnce(action, id, seq)
    }, POLL_INTERVAL_MS)
    timeoutTimer = setTimeout(() => handleTimeout(action, id, seq), TIMEOUT_MS)
  }

  async function run(action: PendingAction, id: string | number): Promise<boolean> {
    // 已有在途操作时不重复受理（按钮 disabled 已兜底，此处防御性拦截）
    if (pending.value !== null) return false
    clearError()
    // 提前占位：HTTP 在途窗口即视为在途（按钮立即转圈，防重复点击双发请求）
    pending.value = { id, action }
    const fn = action === 'start' ? startVM : action === 'stop' ? stopVM : restartVM
    try {
      await fn(id)
    } catch (err) {
      // 受理失败（后端拒绝/网络层）：error toast + 行内错误条，VM 状态不变
      // 已卸载时静默返回：页面不复存在，不再弹 toast / 写错误条
      // 目标校验：路由切换（组件复用）场景下 stop() 已清空 pending，旧 VM 操作的
      // 迟到响应（await 返回后继续执行）被拦截，杜绝误导 toast 与在途状态污染
      if (disposed || pending.value?.id !== id) return false
      pending.value = null
      const apiErr = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
      pendingError.value = apiErr
      toast.add({
        title: `${PENDING_ACTION_LABELS[action]}失败`,
        description: apiErr.message,
        color: 'error',
        icon: 'i-lucide-circle-x'
      })
      return false
    }
    // 已卸载时不再继续：跳过受理 toast / onAccepted / startObserving
    // （否则重建的 interval+timeout 无人清理，最长 90s，且卸载页面仍弹结果 toast）
    // 目标校验：路由切换（组件复用）时 stop() 已清空 pending 并递增 observeSeq，
    // 旧 VM 的迟到响应即使序号巧合命中也会被拦截，直接静默返回——
    // 不再弹 toast / 调 onAccepted / startObserving，杜绝空转轮询与 observeSeq 污染
    if (disposed || pending.value?.id !== id) return false
    // 两段式 toast 第一段：受理 info（design D5）
    const name = options.getVMState(id)?.name
    toast.add({
      title: `正在${PENDING_ACTION_LABELS[action]}…`,
      description: name ? `「${name}」操作已受理，正在等待 PVE 生效` : '操作已受理，正在等待 PVE 生效',
      color: 'info',
      icon: 'i-lucide-loader-circle'
    })
    options.onAccepted?.(id)
    startObserving(action, id)
    return true
  }

  // 组件卸载时清理定时器与在途状态，避免泄漏；disposed 置位拦截卸载后返回的受理响应
  onUnmounted(() => {
    disposed = true
    stop()
  })

  return { pending, pendingError, run, stop, clearError }
}
