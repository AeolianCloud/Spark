<script setup lang="ts">
/**
 * VM 列表页（4.1/4.2/4.4/4.5）：
 * - 分页：limit/offset + X-Total-Count 总数，页码与每页条数由 URL query 驱动（?page=&size=）
 * - 刷新策略（design D5）：手动刷新按钮 + 可选 ≥10s 低频自动刷新；PVE 降级（warnings）展示降级提示
 * - 创建虚拟机：列表页内 UModal 表单，Zone 联动过滤镜像；密码一次性提交不存储不回显
 * - 行内生命周期操作：启动/停止/重启/销毁（销毁二次确认），失败展示后端错误且状态不变
 */
import type { Image, NodeResponse, NodeWarning, Pool, StorageType, UnmanagedVM, VMListItem, ZoneResponse } from '~/api'
import {
  ApiError,
  createVM,
  destroyVM,
  importVM,
  listImages,
  listNodesByZone,
  listPools,
  listStorageTypes,
  listUnmanagedVMs,
  listVMs,
  listZones,
  restartVM,
  startVM,
  stopVM
} from '~/api'
import { useCatalog } from '~/composables/useCatalog'
import { formatMemMB, vmStatusBadge } from '~/utils/vm'

const route = useRoute()
const router = useRouter()
const toast = useToast()
const { refresh: refreshCatalog, zoneName, nodeName } = useCatalog()

// BUG-2 修复：本页存在子路由 /vms/:id，父页面只在自身路由下渲染列表，
// 子路由访问时仅渲染子页面（NuxtPage 出口），避免两个 UDashboardPanel 堆叠
const isParentRoute = computed(() => !route.path.startsWith('/vms/'))

// ---- 分页：URL query 为唯一事实来源（设计 D6），非法值回退默认 ----
const PAGE_SIZES = [10, 25, 50, 100] as const
const DEFAULT_SIZE = 25 // 契约 QueryLimit 默认值

function parsePage(raw: unknown): number {
  const n = Number(raw)
  return Number.isInteger(n) && n >= 1 ? n : 1
}

function parseSize(raw: unknown): number {
  const n = Number(raw)
  return (PAGE_SIZES as readonly number[]).includes(n) ? n : DEFAULT_SIZE
}

const page = computed(() => parsePage(route.query.page))
const size = computed(() => parseSize(route.query.size))

// 翻页：写入 URL query（page=1 时不携带，保持 URL 干净）
function setPage(p: number): void {
  void router.replace({ query: { ...route.query, page: p > 1 ? String(p) : undefined } })
}

// 切换每页条数：同时重置回第 1 页
function setSize(s: number): void {
  void router.replace({ query: { ...route.query, page: undefined, size: s === DEFAULT_SIZE ? undefined : String(s) } })
}

function onSizeChange(v: number | undefined): void {
  if (typeof v === 'number' && v !== size.value) setSize(v)
}

// ---- 列表数据 ----
const vms = ref<VMListItem[]>([])
const total = ref(0)
const warnings = ref<NodeWarning[]>([])
const loading = ref(true)
const error = ref<ApiError | null>(null)

// 请求序号守卫（design：最后意图胜出）：每次发起 ++fetchSeq，
// 响应到达时序号不匹配即过期丢弃；旧请求不再阻塞新请求（原先的 in-flight 丢弃会静默丢最新意图）
let fetchSeq = 0
// 越界回退时置位：loading 不在此复位，交由末页重拉接管（避免空态闪烁）
let keepLoadingOnFallback = false

// 拉取列表：并发请求安全（自动刷新/手动刷新/翻页可同时发起，过期响应一律丢弃）
async function fetchVMs(): Promise<void> {
  const seq = ++fetchSeq
  const pageNow = page.value
  const sizeNow = size.value
  keepLoadingOnFallback = false
  loading.value = true
  error.value = null
  try {
    const res = await listVMs({ limit: sizeNow, offset: (pageNow - 1) * sizeNow })
    // 过期响应丢弃：期间已发起更新的请求（如翻页/刷新），本响应不落盘
    if (seq !== fetchSeq) return
    vms.value = res.data.vms
    total.value = res.total
    warnings.value = res.data.warnings
    // 越界回退：直接跳到真实末页（而非逐页 -1），避免 URL ?page=100000 时串行十万次请求
    const lastPage = Math.max(1, Math.ceil(res.total / sizeNow))
    if (pageNow > lastPage) {
      setPage(lastPage)
      keepLoadingOnFallback = true
      return
    }
  } catch (err) {
    if (seq !== fetchSeq) return
    // 刷新失败保留旧数据与总数（仅展示错误条）；首次加载无数据时自然保留空态
    error.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (seq === fetchSeq && !keepLoadingOnFallback) loading.value = false
  }
}

// ---- 刷新策略（design D5）：手动 + 可选 ≥10s 低频轮询 ----
// PVE 状态为透传查询（每次列表请求按节点扇出），轮询间隔不得低于 10 秒（约束），取 15 秒
const REFRESH_INTERVAL_MS = 15_000
const autoRefresh = ref(false)
let refreshTimer: ReturnType<typeof setInterval> | null = null

watch(autoRefresh, (on) => {
  if (refreshTimer) {
    clearInterval(refreshTimer)
    refreshTimer = null
  }
  if (on) {
    refreshTimer = setInterval(() => {
      // 详情页（/vms/:id）下父页面常驻但列表区域不渲染：跳过轮询，避免对 PVE 的扇出请求空转
      if (!isParentRoute.value) return
      void fetchVMs()
    }, REFRESH_INTERVAL_MS)
  }
})

// 组件卸载时清理定时器，避免泄漏
onUnmounted(() => {
  if (refreshTimer) {
    clearInterval(refreshTimer)
  }
})

// 翻页/每页条数变化（含浏览器前进后退）→ 重新拉取
watch(() => [page.value, size.value], () => {
  void fetchVMs()
})

// 从详情页（/vms/:id）返回列表（/vms）时刷新：父页面常驻不重挂载（onMounted 不触发），
// 否则详情页销毁 VM 后返回列表仍显示已删除的 VM（stale）；
// path 变化与分页 query 变化是两个独立 watch（翻页只改 query 不改 path），互不冲突；
// 刷新复用 fetchSeq 序号守卫（最后意图胜出），与并发请求无竞态
watch(() => route.path, (_path, oldPath) => {
  if (oldPath && oldPath.startsWith('/vms/') && isParentRoute.value) {
    void fetchVMs()
  }
})

onMounted(() => {
  // 子路由访问时父页面不加载列表（列表区域不渲染，避免无谓请求）
  if (!isParentRoute.value) return
  void fetchVMs()
  // 目录映射刷新（失败不阻塞列表，映射缺失时展示 id 并注明）：进入页面即重拉，避免名称映射长期过期
  void refreshCatalog()
})

// ---- PVE 降级提示（不伪造状态：warnings 对应节点 VM 为静态字段值，非实时）----
const degradedDescription = computed(() => {
  if (warnings.value.length === 0) return ''
  const details = warnings.value.map(w => `${w.node}：${w.error}`).join('；')
  return `${details}。该节点 VM 的状态为降级后的静态字段值，非实时。`
})

// ---- Zone/节点 id → 名称（映射未就绪展示 id 并注明，避免误导）----
function zoneLabel(id: number): string {
  return zoneName(id) ?? `Zone #${id}（名称未加载）`
}

function nodeLabel(id: number): string {
  return nodeName(id) ?? `节点 #${id}（名称未加载）`
}

// ---- 行内生命周期操作（4.5）：202 受理即成功；失败展示后端错误且状态不变 ----
const actionBusyId = ref<number | null>(null)
const actionError = ref<ApiError | null>(null)

const ACTION_LABELS = { start: '启动', stop: '关闭', restart: '重启' } as const
type LifecycleAction = keyof typeof ACTION_LABELS

async function runLifecycle(action: LifecycleAction, vm: VMListItem): Promise<void> {
  actionError.value = null
  actionBusyId.value = vm.id
  try {
    const fn = action === 'start' ? startVM : action === 'stop' ? stopVM : restartVM
    await fn(vm.id)
    toast.add({
      title: '操作已受理',
      description: `已提交${ACTION_LABELS[action]}「${vm.name}」，异步生效，可稍后刷新查看最新状态`,
      color: 'success',
      icon: 'i-lucide-check-circle-2'
    })
    // 受理后立即刷新一次，让 PVE 状态尽快可见
    void fetchVMs()
  } catch (err) {
    actionError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    actionBusyId.value = null
  }
}

// ---- 销毁：必须二次确认（输入 VM 名称，警示不可恢复）----
const destroyTarget = ref<VMListItem | null>(null)
const destroyInput = ref('')
const destroyBusy = ref(false)
const destroyError = ref<ApiError | null>(null)

const destroyConfirmDisabled = computed(() => destroyInput.value !== destroyTarget.value?.name)

// 弹窗开关：以 destroyTarget 为准（setter 处理各类关闭路径）
const destroyOpen = computed({
  get: () => destroyTarget.value !== null,
  set: (open: boolean) => {
    if (!open) destroyTarget.value = null
  }
})

function openDestroyConfirm(vm: VMListItem): void {
  destroyTarget.value = vm
  destroyInput.value = ''
  destroyError.value = null
}

async function confirmDestroy(): Promise<void> {
  const target = destroyTarget.value
  if (!target || destroyConfirmDisabled.value || destroyBusy.value) return
  destroyBusy.value = true
  destroyError.value = null
  try {
    await destroyVM(target.id)
    toast.add({ title: '已销毁', description: `虚拟机「${target.name}」已删除`, color: 'success', icon: 'i-lucide-trash-2' })
    destroyTarget.value = null
    // 刷新列表；若当前页被删空，fetchVMs 内部自动回退一页
    void fetchVMs()
  } catch (err) {
    destroyError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    destroyBusy.value = false
  }
}

// ---- 创建虚拟机（4.4）：列表页 UModal 表单 ----
const createOpen = ref(false)
const creating = ref(false)
const createError = ref<ApiError | null>(null)
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const createFormRef = ref<{ submit: () => Promise<void> }>()

// 表单状态：密码不在此对象中，仅由独立 ref 承载并在提交时即弃（安全约束）
const createForm = reactive({
  name: '',
  cpu: 1,
  mem_mb: 2048,
  disk_gb: 20,
  zone_id: undefined as number | undefined,
  storage_type_id: undefined as number | undefined,
  image_id: undefined as number | undefined
})

// 注入密码：仅用于提交，提交后立即清空，不持久化/不回显/不打印
const password = ref('')

// 表单选项（打开 modal 时按需加载；失败给出轻量提示，下拉为空不再静默）
const zones = ref<ZoneResponse[]>([])
const storageTypes = ref<StorageType[]>([])
const images = ref<Image[]>([])
const imagesLoading = ref(false)
const zonesLoadError = ref<ApiError | null>(null)
const storageTypesLoadError = ref<ApiError | null>(null)
const imagesLoadError = ref<ApiError | null>(null)

// 内存快捷档位（MB）
const MEM_PRESETS = [1024, 2048, 4096, 8192]

const zoneOptions = computed(() => zones.value.map(z => ({ label: z.name, value: z.id })))
const storageTypeOptions = computed(() => storageTypes.value.map(s => ({ label: `${s.display_name}（${s.name}）`, value: s.id })))
const imageOptions = computed(() => images.value.map(i => ({ label: `${i.name}（默认用户 ${i.default_user}）`, value: i.id })))

// 镜像列表请求序号守卫：Zone 快速切换时丢弃过期响应，防止旧 Zone 的镜像覆盖新 Zone
let imagesSeq = 0

// 可用区切换 → 镜像按该 Zone 过滤（契约 listImages 支持 zone_id；镜像可用性依赖 Zone）
watch(() => createForm.zone_id, async (zoneId) => {
  createForm.image_id = undefined // 切换 Zone 后原镜像可能不可用，强制重新选择
  images.value = []
  imagesLoadError.value = null
  if (zoneId === undefined) return
  const seq = ++imagesSeq
  imagesLoading.value = true
  try {
    const res = await listImages({ zone_id: zoneId, limit: 100 })
    // 过期响应丢弃：期间 Zone 已再次切换
    if (seq !== imagesSeq || zoneId !== createForm.zone_id) return
    images.value = res.data
  } catch (err) {
    if (seq !== imagesSeq || zoneId !== createForm.zone_id) return
    imagesLoadError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (seq === imagesSeq) imagesLoading.value = false
  }
})

// 可用区列表懒加载（创建/导入两个弹窗共用；加载失败记录错误 ref，弹窗内给出轻量提示，重新打开会重试）
async function loadZones(): Promise<void> {
  if (zones.value.length > 0) return
  zonesLoadError.value = null
  try {
    zones.value = (await listZones({ limit: 100 })).data
  } catch (err) {
    zonesLoadError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
    zones.value = []
  }
}

// 打开创建弹窗：选项懒加载（Zone/存储类型全局一次；镜像随后续 Zone 选择联动加载）
// 加载失败不吞掉：记录错误 ref，弹窗内给出轻量提示（重新打开弹窗会重试）
async function openCreateModal(): Promise<void> {
  createError.value = null
  createOpen.value = true
  await loadZones()
  if (storageTypes.value.length === 0) {
    storageTypesLoadError.value = null
    try {
      storageTypes.value = (await listStorageTypes({ limit: 100 })).data
    } catch (err) {
      storageTypesLoadError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
      storageTypes.value = []
    }
  }
}

// 表单校验：必填项 + 数字下限（UForm 无 schema 场景，自定义校验函数）
function validateCreateForm(): { name?: string, message: string }[] {
  const errors: { name?: string, message: string }[] = []
  if (!createForm.name.trim()) errors.push({ name: 'name', message: '请输入 VM 名称' })
  if (!Number.isInteger(createForm.cpu) || createForm.cpu < 1) errors.push({ name: 'cpu', message: 'vCPU 至少为 1' })
  if (!Number.isInteger(createForm.mem_mb) || createForm.mem_mb < 1) errors.push({ name: 'mem_mb', message: '内存至少为 1 MB' })
  if (!Number.isInteger(createForm.disk_gb) || createForm.disk_gb < 1) errors.push({ name: 'disk_gb', message: '磁盘至少为 1 GB' })
  if (createForm.zone_id === undefined) errors.push({ name: 'zone_id', message: '请选择可用区' })
  if (createForm.storage_type_id === undefined) errors.push({ name: 'storage_type_id', message: '请选择存储类型' })
  if (createForm.image_id === undefined) errors.push({ name: 'image_id', message: '请选择镜像' })
  if (!password.value) errors.push({ name: 'password', message: '请输入注入密码' })
  return errors
}

// 提交创建：密码读取后立即清空（无论请求成败都不再保留），不写入任何本地状态
async function submitCreate(): Promise<void> {
  if (creating.value) return
  // 防御性二次校验（UForm 校验之外的最后防线）：不通过则不发起请求、不清空密码
  const residualErrors = validateCreateForm()
  if (residualErrors.length > 0) {
    createError.value = new ApiError(400, 'bad_request', residualErrors.map(e => e.message).join('；'))
    return
  }
  creating.value = true
  createError.value = null
  const pwd = password.value
  password.value = '' // 立即清空：密码仅存于本次提交的局部变量（security-reviewer 复核点）
  try {
    // 校验已保证 zone_id/storage_type_id/image_id 非空
    await createVM({
      name: createForm.name.trim(),
      cpu: createForm.cpu,
      mem_mb: createForm.mem_mb,
      disk_gb: createForm.disk_gb,
      image_id: createForm.image_id!,
      storage_type_id: createForm.storage_type_id!,
      zone_id: createForm.zone_id!,
      password: pwd
    })
    // 成功后清空整个表单并关闭弹窗，刷新列表可见新 VM（status 为 creating）
    createForm.name = ''
    createForm.cpu = 1
    createForm.mem_mb = 2048
    createForm.disk_gb = 20
    createForm.zone_id = undefined
    createForm.storage_type_id = undefined
    createForm.image_id = undefined
    images.value = []
    createOpen.value = false
    toast.add({ title: '创建已受理', description: '虚拟机创建中，稍后刷新可见', color: 'success', icon: 'i-lucide-check-circle-2' })
    void fetchVMs()
  } catch (err) {
    // 契约错误（含 400 image_not_available_in_zone）由 AppErrorAlert 展示错误码与后端描述
    createError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    creating.value = false
  }
}

// 关闭弹窗时清空密码：即使未提交也不留存在组件状态中（表单其余字段保留，便于再次打开）
watch(createOpen, (open) => {
  if (!open) password.value = ''
})

// ---- 导入已有 VM：把 PVE 上已存在的 VM 纳入平台纳管（无密码字段） ----
const importOpen = ref(false)
const importing = ref(false)
const importError = ref<ApiError | null>(null)
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const importFormRef = ref<{ submit: () => Promise<void> }>()

// 表单状态：pve_vmid 即候选 VM 的 vmid；ip_pool_id/name 可选
const importForm = reactive({
  zone_id: undefined as number | undefined,
  node_id: undefined as number | undefined,
  pve_vmid: undefined as number | undefined,
  ip_pool_id: undefined as number | undefined,
  name: ''
})

// 联动选项（按需加载；加载失败记录错误 ref，下拉内给出轻量提示而非静默）
const nodes = ref<NodeResponse[]>([])
const unmanagedVMs = ref<UnmanagedVM[]>([])
const pools = ref<Pool[]>([])
const nodesLoading = ref(false)
const candidatesLoading = ref(false)
const poolsLoading = ref(false)
const nodesLoadError = ref<ApiError | null>(null)
const candidatesLoadError = ref<ApiError | null>(null)
const poolsLoadError = ref<ApiError | null>(null)

const nodeOptions = computed(() => nodes.value.map(n => ({ label: n.name, value: n.id })))
const candidateOptions = computed(() => unmanagedVMs.value.map(vm => ({
  label: `${vm.name}（VMID ${vm.vmid}，${vm.status}）`,
  value: vm.vmid
})))
const poolOptions = computed(() => pools.value.map(p => ({ label: p.name, value: p.id })))

// 请求序号守卫：可用区/节点快速切换时丢弃过期响应，防止旧联动结果覆盖新联动。
// 注意：nodes/pools 两条链必须各自独立计数（参考创建弹窗 imagesSeq），
// 若共用同一计数器，zone watch 内同步顺序调用会让后发起者抢走序号，
// 先发起者的响应到达时序号已不匹配，恒被当作过期丢弃（节点下拉永远加载不出）
let nodesSeq = 0 // 可用区联动（节点）响应守卫
let poolsSeq = 0 // 可用区联动（IP 池）响应守卫
let candidateSeq = 0 // 节点联动（候选 VM）响应守卫

async function loadNodes(zoneId: number): Promise<void> {
  const seq = ++nodesSeq
  nodesLoading.value = true
  nodesLoadError.value = null
  try {
    const res = await listNodesByZone(zoneId)
    // 过期响应丢弃：期间可用区已再次切换
    if (seq !== nodesSeq || zoneId !== importForm.zone_id) return
    nodes.value = res.data
  } catch (err) {
    if (seq !== nodesSeq || zoneId !== importForm.zone_id) return
    nodesLoadError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (seq === nodesSeq) nodesLoading.value = false
  }
}

async function loadPools(zoneId: number): Promise<void> {
  const seq = ++poolsSeq
  poolsLoading.value = true
  poolsLoadError.value = null
  try {
    // listPools 契约支持 zone_id 过滤，直接按可用区拉取
    const res = await listPools({ zone_id: zoneId, limit: 100 })
    if (seq !== poolsSeq || zoneId !== importForm.zone_id) return
    pools.value = res.data
  } catch (err) {
    if (seq !== poolsSeq || zoneId !== importForm.zone_id) return
    poolsLoadError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (seq === poolsSeq) poolsLoading.value = false
  }
}

async function loadCandidates(nodeId: number): Promise<void> {
  const seq = ++candidateSeq
  candidatesLoading.value = true
  candidatesLoadError.value = null
  try {
    const res = await listUnmanagedVMs({ node_id: nodeId })
    // 过期响应丢弃：期间节点已再次切换
    if (seq !== candidateSeq || nodeId !== importForm.node_id) return
    unmanagedVMs.value = res.data.vms
  } catch (err) {
    if (seq !== candidateSeq || nodeId !== importForm.node_id) return
    candidatesLoadError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (seq === candidateSeq) candidatesLoading.value = false
  }
}

// 可用区切换 → 重置节点/候选/IP 池（原选择可能不可用），并行按该 Zone 加载节点与 IP 池
watch(() => importForm.zone_id, (zoneId) => {
  importForm.node_id = undefined
  importForm.pve_vmid = undefined
  importForm.ip_pool_id = undefined
  nodes.value = []
  unmanagedVMs.value = []
  pools.value = []
  nodesLoadError.value = null
  candidatesLoadError.value = null
  poolsLoadError.value = null
  if (zoneId === undefined) return
  void loadNodes(zoneId)
  void loadPools(zoneId)
})

// 节点切换 → 重置候选 VM（原候选可能不可用），按该节点加载候选
watch(() => importForm.node_id, (nodeId) => {
  importForm.pve_vmid = undefined
  unmanagedVMs.value = []
  candidatesLoadError.value = null
  if (nodeId === undefined) return
  void loadCandidates(nodeId)
})

// 打开导入弹窗：可用区选项懒加载（与创建弹窗共用 zones ref）
async function openImportModal(): Promise<void> {
  importError.value = null
  importOpen.value = true
  await loadZones()
}

// 弹窗关闭（取消）时重置表单与联动状态：避免上次的 zone/node/候选选择残留，
// 且再次打开时联动选项过期（PVE 侧可能已变化），应清空后按新选择重新拉取。
// 成功路径 submitImport 已自行重置，此处统一兜底，逻辑与创建弹窗的 watch(createOpen) 一致
watch(importOpen, (open) => {
  if (open) return
  importForm.zone_id = undefined
  importForm.node_id = undefined
  importForm.pve_vmid = undefined
  importForm.ip_pool_id = undefined
  importForm.name = ''
  nodes.value = []
  unmanagedVMs.value = []
  pools.value = []
  importError.value = null
  nodesLoadError.value = null
  candidatesLoadError.value = null
  poolsLoadError.value = null
})

// 表单校验：可用区/节点/候选 VM 必选（IP 池与名称可选）
// 名称上限 128 字符与后端契约 ImportVMRequest.name maxLength 对齐；留空时不做长度校验
function validateImportForm(): { name?: string, message: string }[] {
  const errors: { name?: string, message: string }[] = []
  if (importForm.zone_id === undefined) errors.push({ name: 'zone_id', message: '请选择可用区' })
  if (importForm.node_id === undefined) errors.push({ name: 'node_id', message: '请选择节点' })
  if (importForm.pve_vmid === undefined) errors.push({ name: 'pve_vmid', message: '请选择要导入的虚拟机' })
  if (importForm.name.trim().length > 128) errors.push({ name: 'name', message: '名称最多 128 字符' })
  return errors
}

// 提交导入：候选 VM 的 vmid 即 pve_vmid；名称留空时不传（使用 PVE 侧名称）
async function submitImport(): Promise<void> {
  if (importing.value) return
  // 防御性二次校验（UForm 校验之外的最后防线）：不通过则不发起请求
  const residualErrors = validateImportForm()
  if (residualErrors.length > 0) {
    importError.value = new ApiError(400, 'bad_request', residualErrors.map(e => e.message).join('；'))
    return
  }
  importing.value = true
  importError.value = null
  try {
    // 校验已保证 zone_id/node_id/pve_vmid 非空
    await importVM({
      zone_id: importForm.zone_id!,
      node_id: importForm.node_id!,
      pve_vmid: importForm.pve_vmid!,
      ip_pool_id: importForm.ip_pool_id,
      name: importForm.name.trim() || undefined
    })
    // 成功后清空表单并关闭弹窗，刷新列表可见新纳入的 VM
    importForm.zone_id = undefined
    importForm.node_id = undefined
    importForm.pve_vmid = undefined
    importForm.ip_pool_id = undefined
    importForm.name = ''
    nodes.value = []
    unmanagedVMs.value = []
    pools.value = []
    importOpen.value = false
    toast.add({ title: '已导入', description: '虚拟机已纳入纳管，可刷新查看最新状态', color: 'success', icon: 'i-lucide-check-circle-2' })
    void fetchVMs()
  } catch (err) {
    // 契约错误（含 vm_not_found_on_node / vm_already_managed / ip_exhausted 等）由 AppErrorAlert 展示错误码与后端描述
    importError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    importing.value = false
  }
}

// 表格列定义（accessorKey 对应 VMListItem 字段，插槽按列 id 覆盖渲染）
const columns = [
  { accessorKey: 'name', header: '名称' },
  { accessorKey: 'cpu', header: 'vCPU' },
  { accessorKey: 'mem_mb', header: '内存' },
  { accessorKey: 'disk_gb', header: '磁盘' },
  { accessorKey: 'status', header: '状态' },
  { accessorKey: 'zone_id', header: '可用区' },
  { accessorKey: 'node_id', header: '节点' },
  { accessorKey: 'ip', header: 'IP' },
  { accessorKey: 'id', header: '操作' }
]

const sizeOptions: { label: string, value: number }[] = PAGE_SIZES.map(s => ({ label: `${s} 条/页`, value: s }))
</script>

<template>
  <UDashboardPanel v-if="isParentRoute">
    <template #header>
      <UDashboardNavbar title="虚拟机">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <div class="space-y-4 p-4">
        <!-- 工具栏：手动刷新 + 可选自动刷新 + 创建入口 -->
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div class="flex items-center gap-4">
            <UButton
              icon="i-lucide-refresh-cw"
              variant="outline"
              :loading="loading"
              @click="fetchVMs"
            >
              刷新
            </UButton>
            <label class="flex cursor-pointer items-center gap-2 text-sm text-muted">
              <USwitch
                v-model="autoRefresh"
                size="sm"
              />
              自动刷新（15 秒）
            </label>
          </div>
          <div class="flex items-center gap-2">
            <UButton
              icon="i-lucide-import"
              variant="outline"
              @click="openImportModal"
            >
              导入 VM
            </UButton>
            <UButton
              icon="i-lucide-plus"
              color="primary"
              @click="openCreateModal"
            >
              创建虚拟机
            </UButton>
          </div>
        </div>

        <!-- PVE 降级提示：节点查询失败，状态为降级后的静态字段值，非实时（不伪造状态） -->
        <UAlert
          v-if="warnings.length > 0"
          color="warning"
          variant="subtle"
          icon="i-lucide-triangle-alert"
          title="部分节点状态降级"
          :description="degradedDescription"
        />

        <!-- 列表拉取失败（含网络层失败，后端未启动时预期） -->
        <AppErrorAlert
          v-if="error"
          :code="error.code"
          :message="error.message"
          title="列表加载失败"
        />

        <!-- 行内操作失败：展示后端错误，VM 状态不变 -->
        <AppErrorAlert
          v-if="actionError"
          :code="actionError.code"
          :message="actionError.message"
          title="操作失败"
        />

        <!-- 加载态骨架屏：仅首次加载（无数据）时显示；刷新/翻页保留旧表格，避免每 15s 闪烁 -->
        <AppLoading
          v-if="vms.length === 0 && loading"
          :rows="8"
        />

        <template v-else>
          <AppEmpty
            v-if="vms.length === 0"
            title="暂无虚拟机"
            description="点击右上角「创建虚拟机」开始使用。"
            icon="i-lucide-monitor"
          />

          <UTable
            v-else
            :data="vms"
            :columns="columns"
          >
            <!-- 名称：详情链接 + failed 时行内展示 provision_error -->
            <template #name-cell="{ row }">
              <div class="min-w-40">
                <NuxtLink
                  :to="`/vms/${row.original.id}`"
                  class="font-medium text-primary hover:underline"
                >
                  {{ row.original.name }}
                </NuxtLink>
                <p
                  v-if="row.original.provision_error"
                  class="mt-0.5 truncate text-xs text-error"
                  :title="row.original.provision_error"
                >
                  {{ row.original.provision_error }}
                </p>
              </div>
            </template>

            <template #mem_mb-cell="{ row }">
              {{ formatMemMB(row.original.mem_mb) }}
            </template>

            <template #disk_gb-cell="{ row }">
              {{ row.original.disk_gb }} GB
            </template>

            <!-- 状态徽章：未知状态中性样式 -->
            <template #status-cell="{ row }">
              <UBadge
                :color="vmStatusBadge(row.original.status).color"
                variant="soft"
                :label="vmStatusBadge(row.original.status).label"
              />
            </template>

            <template #zone_id-cell="{ row }">
              {{ zoneLabel(row.original.zone_id) }}
            </template>

            <template #node_id-cell="{ row }">
              {{ nodeLabel(row.original.node_id) }}
            </template>

            <template #ip-cell="{ row }">
              {{ row.original.ip ?? '—' }}
            </template>

            <!-- 操作列（显隐+禁用混合口径）：启动/停止/重启按状态显隐（v-if，creating/failed 无按钮）；
             销毁恒显（alwaysShowDestroy），仅状态可销毁时可用（后端 vm_not_ready 兜底） -->
            <template #id-cell="{ row }">
              <div class="flex items-center gap-1">
                <UButton
                  size="sm"
                  variant="ghost"
                  color="primary"
                  icon="i-lucide-eye"
                  :to="`/vms/${row.original.id}`"
                >
                  详情
                </UButton>
                <VmActions
                  :status="row.original.status"
                  :busy="actionBusyId === row.original.id"
                  :busy-any="actionBusyId !== null"
                  size="sm"
                  variant="ghost"
                  always-show-destroy
                  @start="runLifecycle('start', row.original)"
                  @stop="runLifecycle('stop', row.original)"
                  @restart="runLifecycle('restart', row.original)"
                  @destroy="openDestroyConfirm(row.original)"
                />
              </div>
            </template>
          </UTable>

          <!-- 分页：总数来自 X-Total-Count，页码/每页条数读写 URL query；
          按 total 判定显示（某节点宕机导致当前页为空但 total>0 时仍可翻页） -->
          <div
            v-if="total > 0"
            class="flex flex-wrap items-center justify-between gap-3"
          >
            <span class="text-sm text-muted">共 {{ total }} 台</span>
            <div class="flex items-center gap-3">
              <USelect
                :model-value="size"
                :items="sizeOptions"
                class="w-32"
                aria-label="每页条数"
                @update:model-value="onSizeChange"
              />
              <UPagination
                :page="page"
                :total="total"
                :items-per-page="size"
                show-edges
                @update:page="setPage"
              />
            </div>
          </div>
        </template>
      </div>

      <!-- BUG-1：UModal 默认插槽是 DialogTrigger（as-child 内联渲染），内容放默认插槽会内联铺在页面上、
           点击提交还会连带触发打开空弹窗；因此弹窗内容必须进 #body、按钮进 #footer。
           UModal 位于 panel 的 #body 具名插槽末尾（panel 默认插槽为空，不触发插槽冲突）；
           且 UModal 默认 portal 渲染到 body，DOM 位置不影响表现，弹窗只在点击按钮后打开。
           lint 约束：vue/no-multiple-template-root 禁止多根，UModal 无法作为 panel 的兄弟根。
           BUG-3：v-model 改为 v-model:open（UModal 模型为 open/update:open） -->
      <!-- BUG-1：UModal 移出 panel 作为兄弟节点（默认插槽是 DialogTrigger，内容必须进具名插槽） -->
      <UModal
        v-model:open="createOpen"
        title="创建虚拟机"
        description="创建请求异步受理，完成后状态自动流转"
        :dismissible="!creating"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <UForm
            ref="createFormRef"
            :state="createForm"
            :validate="validateCreateForm"
            class="space-y-4"
            @submit="submitCreate"
          >
            <UFormField
              name="name"
              label="名称"
              required
            >
              <UInput
                v-model="createForm.name"
                placeholder="如 web-01"
                autocomplete="off"
              />
            </UFormField>

            <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
              <UFormField
                name="cpu"
                label="vCPU 核数"
                required
              >
                <UInputNumber
                  :model-value="createForm.cpu"
                  :min="1"
                  :step="1"
                  class="w-full"
                  @update:model-value="(v) => { createForm.cpu = v ?? 1 }"
                />
              </UFormField>
              <UFormField
                name="mem_mb"
                label="内存（MB）"
                required
              >
                <UInputNumber
                  :model-value="createForm.mem_mb"
                  :min="1"
                  :step="1024"
                  class="w-full"
                  @update:model-value="(v) => { createForm.mem_mb = v ?? 1 }"
                />
              </UFormField>
              <UFormField
                name="disk_gb"
                label="磁盘（GB）"
                required
              >
                <UInputNumber
                  :model-value="createForm.disk_gb"
                  :min="1"
                  :step="1"
                  class="w-full"
                  @update:model-value="(v) => { createForm.disk_gb = v ?? 1 }"
                />
              </UFormField>
            </div>

            <!-- 内存常用档位快捷选择 -->
            <div class="flex items-center gap-2">
              <span class="text-xs text-muted">内存快捷档位：</span>
              <UButton
                v-for="m in MEM_PRESETS"
                :key="m"
                size="xs"
                variant="outline"
                :color="createForm.mem_mb === m ? 'primary' : 'neutral'"
                @click="createForm.mem_mb = m"
              >
                {{ m / 1024 }} GB
              </UButton>
            </div>

            <UFormField
              name="zone_id"
              label="可用区"
              required
              :description="zonesLoadError ? `可用区列表加载失败（${zonesLoadError.code}），请关闭弹窗后重新打开重试` : undefined"
            >
              <USelect
                :model-value="createForm.zone_id"
                :items="zoneOptions"
                placeholder="选择可用区"
                class="w-full"
                @update:model-value="(v: number | undefined) => { createForm.zone_id = v }"
              />
            </UFormField>

            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <UFormField
                name="storage_type_id"
                label="存储类型"
                required
                :description="storageTypesLoadError ? `存储类型加载失败（${storageTypesLoadError.code}），请关闭弹窗后重新打开重试` : undefined"
              >
                <USelect
                  :model-value="createForm.storage_type_id"
                  :items="storageTypeOptions"
                  placeholder="选择存储类型"
                  class="w-full"
                  @update:model-value="(v: number | undefined) => { createForm.storage_type_id = v }"
                />
              </UFormField>
              <UFormField
                name="image_id"
                label="镜像"
                required
                :description="imagesLoadError
                  ? `镜像列表加载失败（${imagesLoadError.code}），请重新选择可用区重试`
                  : (createForm.zone_id === undefined ? '请先选择可用区以加载镜像' : '已按所选可用区过滤')"
              >
                <USelect
                  :model-value="createForm.image_id"
                  :items="imageOptions"
                  :loading="imagesLoading"
                  :disabled="createForm.zone_id === undefined"
                  placeholder="选择镜像"
                  class="w-full"
                  @update:model-value="(v: number | undefined) => { createForm.image_id = v }"
                />
              </UFormField>
            </div>

            <UFormField
              name="password"
              label="注入密码"
              required
              description="仅用于 cloud-init 一次性注入，提交后立即丢弃，不存储不回显"
            >
              <UInput
                v-model="password"
                type="password"
                autocomplete="new-password"
                placeholder="输入创建时注入的密码"
              />
            </UFormField>
          </UForm>

          <!-- 创建失败（含 image_not_available_in_zone 等契约错误码） -->
          <AppErrorAlert
            v-if="createError"
            class="mt-4"
            :code="createError.code"
            :message="createError.message"
            title="创建失败"
          />
        </template>

        <template #footer>
          <UButton
            variant="outline"
            :disabled="creating"
            @click="createOpen = false"
          >
            取消
          </UButton>
          <UButton
            color="primary"
            :loading="creating"
            @click="createFormRef?.submit()"
          >
            创建
          </UButton>
        </template>
      </UModal>

      <!-- BUG-1：UModal 移出 panel 作为兄弟节点 -->
      <UModal
        v-model:open="destroyOpen"
        title="销毁虚拟机"
        description="该操作不可恢复，请输入虚拟机名称确认"
        :dismissible="!destroyBusy"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <div class="space-y-4">
            <UAlert
              color="error"
              variant="subtle"
              icon="i-lucide-trash-2"
              title="不可恢复"
              description="虚拟机及其磁盘数据将被永久删除（含 PVE 侧 purge），无法恢复。"
            />
            <UInput
              v-model="destroyInput"
              :placeholder="`输入「${destroyTarget?.name ?? ''}」以确认`"
              autocomplete="off"
            />
            <AppErrorAlert
              v-if="destroyError"
              :code="destroyError.code"
              :message="destroyError.message"
              title="销毁失败"
            />
          </div>
        </template>

        <template #footer>
          <UButton
            variant="outline"
            :disabled="destroyBusy"
            @click="destroyTarget = null"
          >
            取消
          </UButton>
          <UButton
            color="error"
            :loading="destroyBusy"
            :disabled="destroyConfirmDisabled"
            @click="confirmDestroy"
          >
            确认销毁
          </UButton>
        </template>
      </UModal>

      <!-- BUG-1：UModal 移出 panel 作为兄弟节点；导入把 PVE 已有 VM 纳入纳管，无密码字段 -->
      <UModal
        v-model:open="importOpen"
        title="导入 VM"
        description="将 PVE 上已有的虚拟机纳入平台纳管"
        :dismissible="!importing"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <UForm
            ref="importFormRef"
            :state="importForm"
            :validate="validateImportForm"
            class="space-y-4"
            @submit="submitImport"
          >
            <UFormField
              name="zone_id"
              label="可用区"
              required
              :description="zonesLoadError ? `可用区列表加载失败（${zonesLoadError.code}），请关闭弹窗后重新打开重试` : undefined"
            >
              <USelect
                :model-value="importForm.zone_id"
                :items="zoneOptions"
                placeholder="选择可用区"
                class="w-full"
                @update:model-value="(v: number | undefined) => { importForm.zone_id = v }"
              />
            </UFormField>

            <UFormField
              name="node_id"
              label="节点"
              required
              :description="nodesLoadError
                ? `节点列表加载失败（${nodesLoadError.code}），请重新选择可用区重试`
                : (importForm.zone_id === undefined ? '请先选择可用区以加载节点' : '已按所选可用区过滤')"
            >
              <USelect
                :model-value="importForm.node_id"
                :items="nodeOptions"
                :loading="nodesLoading"
                :disabled="importForm.zone_id === undefined"
                placeholder="选择节点"
                class="w-full"
                @update:model-value="(v: number | undefined) => { importForm.node_id = v }"
              />
            </UFormField>

            <UFormField
              name="pve_vmid"
              label="待导入虚拟机"
              required
              :description="candidatesLoadError
                ? `候选列表加载失败（${candidatesLoadError.code}），请重新选择节点重试`
                : (importForm.node_id === undefined ? '请先选择节点以加载候选' : '仅展示该节点上未被纳管的 VM')"
            >
              <USelect
                :model-value="importForm.pve_vmid"
                :items="candidateOptions"
                :loading="candidatesLoading"
                :disabled="importForm.node_id === undefined"
                placeholder="选择要导入的虚拟机"
                class="w-full"
                @update:model-value="(v: number | undefined) => { importForm.pve_vmid = v }"
              />
            </UFormField>

            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <UFormField
                name="ip_pool_id"
                label="IP 池"
                :description="poolsLoadError
                  ? `IP 池加载失败（${poolsLoadError.code}），请重新选择可用区重试`
                  : (importForm.zone_id === undefined ? '请先选择可用区以加载 IP 池' : '留空自动分配')"
              >
                <USelect
                  :model-value="importForm.ip_pool_id"
                  :items="poolOptions"
                  :loading="poolsLoading"
                  :disabled="importForm.zone_id === undefined"
                  placeholder="自动分配"
                  class="w-full"
                  @update:model-value="(v: number | undefined) => { importForm.ip_pool_id = v }"
                />
              </UFormField>

              <UFormField
                name="name"
                label="名称"
              >
                <UInput
                  v-model="importForm.name"
                  maxlength="128"
                  placeholder="留空使用 PVE 上的名称"
                  autocomplete="off"
                />
              </UFormField>
            </div>
          </UForm>

          <!-- 导入失败（含 vm_not_found_on_node / vm_already_managed / ip_exhausted 等契约错误码） -->
          <AppErrorAlert
            v-if="importError"
            class="mt-4"
            :code="importError.code"
            :message="importError.message"
            title="导入失败"
          />
        </template>

        <template #footer>
          <UButton
            variant="outline"
            :disabled="importing"
            @click="importOpen = false"
          >
            取消
          </UButton>
          <UButton
            color="primary"
            :loading="importing"
            @click="importFormRef?.submit()"
          >
            导入
          </UButton>
        </template>
      </UModal>
    </template>
  </UDashboardPanel>

  <NuxtPage v-else />
</template>
