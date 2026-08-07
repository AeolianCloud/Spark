<script setup lang="ts">
/**
 * 镜像管理页：列表 + 登记 + 下载 + 操作历史。
 * 契约适配（image-auto-discovery-and-download）：
 * - Image 不再携带 node_images（节点名 → 路径对象），改为 download_url + 节点状态实时扫描；
 * - 节点状态（已下载/未下载）由 GET /images/:id/nodes-status 实时扫描：无区域模式下行内并行拉取，
 *   区域过滤模式下由 listImagesByZone 内嵌 ImageZoneItem.nodes；
 * - 下载通过 POST /images/:id/download 异步受理（202，node_ids 与 zone_id 二选一），
 *   进度在操作记录（GET /images/:id/operations）中查看，同一镜像同一节点已有 running 记录时 409。
 */
import { createImage, downloadImage, getImageNodeStatus, listImageOperations, listImages, listImagesByZone } from '~/api/images'
import { listZones } from '~/api/zones'
import { useCatalog } from '~/composables/useCatalog'
import { formatDateTime, type FormValidateError } from '~/utils/format'
import { ApiError } from '~/api/client'
import type { Image, ImageOperation, NodeImageStatus, ZoneResponse } from '~/api/types'

const toast = useToast()
// 目录映射：操作记录展示节点名兜底（行内 nodes 优先，映射缺失回退节点 #id）
const { nodeName: catalogNodeName, ensureLoaded: ensureCatalogLoaded } = useCatalog()

// ---- 列表状态 ----
// 一次拉取上限 100（契约 limit 上限），超过时按第一页口径展示，总数以 X-Total-Count 为准
const PAGE_LIMIT = 100
const loading = ref(true)
const error = ref<ApiError | null>(null)
const total = ref(0)

// 行模型：image 元数据 + 节点状态（无区域模式行挂载后并行扫描；区域模式内嵌）
interface ImageRow {
  image: Image
  nodes: NodeImageStatus[]
  /** 节点状态扫描中（仅无区域模式；区域模式内嵌无此阶段） */
  loading: boolean
  /** 节点状态扫描失败（仅无区域模式；后端单节点失败降级为未下载不整体失败） */
  error: ApiError | null
}

const rows = ref<ImageRow[]>([])

// ---- 区域过滤（可选能力）：undefined = 全部区域（无区域模式） ----
const zones = ref<ZoneResponse[]>([])
const zoneFilter = ref<number | undefined>(undefined)
// USelect 以 0 代表"全部区域"（区域 id 恒为正整数，0 不冲突）
const zoneOptions = computed(() => [
  { label: '全部区域', value: 0 },
  ...zones.value.map(z => ({ label: z.name, value: z.id }))
])

function onZoneFilterChange(value: number | undefined): void {
  zoneFilter.value = value === undefined || value === 0 ? undefined : value
}

async function loadZones(): Promise<void> {
  try {
    zones.value = (await listZones({ limit: PAGE_LIMIT })).data
  } catch {
    // 区域列表加载失败不阻塞列表：仅"全部区域"可用（区域过滤为可选能力）
    zones.value = []
  }
}

// ---- 列表加载：请求序号守卫（最后意图胜出），过期响应一律丢弃 ----
let loadGen = 0

async function load(): Promise<void> {
  const gen = ++loadGen
  loading.value = true
  error.value = null
  try {
    if (zoneFilter.value === undefined) {
      // 无区域模式：全部镜像；每行并行扫描全部启用节点状态
      const res = await listImages({ limit: PAGE_LIMIT })
      if (gen !== loadGen) return
      total.value = res.total
      rows.value = res.data.map(image => ({ image, nodes: [], loading: true, error: null }))
      for (const row of rows.value) void loadRowStatus(row, gen)
    } else {
      // 区域模式：listImagesByZone 内嵌该区域启用节点状态，无需行内再查
      const res = await listImagesByZone(zoneFilter.value, { limit: PAGE_LIMIT })
      if (gen !== loadGen) return
      total.value = res.total
      rows.value = res.data.map(item => ({ image: item.image, nodes: item.nodes, loading: false, error: null }))
    }
  } catch (err) {
    if (gen !== loadGen) return
    error.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (gen === loadGen) loading.value = false
  }
}

/** 单行节点状态扫描（行内并行调用，互不阻塞）；列表重新加载后过期结果丢弃（gen 守卫） */
async function loadRowStatus(row: ImageRow, gen: number): Promise<void> {
  row.loading = true
  row.error = null
  try {
    const res = await getImageNodeStatus(row.image.id)
    if (gen !== loadGen) return
    row.nodes = res.data
  } catch (err) {
    if (gen !== loadGen) return
    row.error = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (gen === loadGen) row.loading = false
  }
}

watch(zoneFilter, () => {
  void load()
})

onMounted(() => {
  void load()
  void loadZones()
  // 目录映射：操作记录节点名兜底（加载失败静默，回退节点 #id）
  void ensureCatalogLoaded()
})

// ---- 节点状态徽标辅助 ----
/** 徽标文案：节点名 + 已下载/未下载 */
function nodeBadgeLabel(node: NodeImageStatus): string {
  return `${node.node_name} · ${node.downloaded ? '已下载' : '未下载'}`
}

/** 徽标悬浮全文：已下载展示匹配卷 ID（如 local:import/xxx.qcow2），未下载给出提示 */
function nodeBadgeTitle(node: NodeImageStatus): string {
  return node.downloaded ? (node.volid ?? '该节点已存在该镜像') : '该节点尚未下载该镜像'
}

// ---- 登记镜像表单：name / default_user / download_url ----
const createOpen = ref(false)
const creating = ref(false)
const createError = ref<ApiError | null>(null)
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const createFormRef = ref<{ submit: () => Promise<void> }>()
const createForm = reactive({
  name: '',
  default_user: '',
  download_url: ''
})

// 下载地址校验：http(s) 必填（协议/域名白名单由服务端最终把关）
const HTTP_URL_RE = /^https?:\/\/\S+$/i

function validateCreate(): FormValidateError[] {
  const errors: FormValidateError[] = []
  if (!createForm.name.trim()) errors.push({ name: 'name', message: '请输入镜像名' })
  if (!createForm.default_user.trim()) errors.push({ name: 'default_user', message: '请输入默认登录用户' })
  const url = createForm.download_url.trim()
  if (!url) {
    errors.push({ name: 'download_url', message: '请输入镜像下载地址' })
  } else if (!HTTP_URL_RE.test(url)) {
    errors.push({ name: 'download_url', message: '下载地址需为 http(s):// 开头的 URL' })
  }
  return errors
}

function resetCreateForm(): void {
  createForm.name = ''
  createForm.default_user = ''
  createForm.download_url = ''
  createError.value = null
}

function openCreateModal(): void {
  resetCreateForm()
  createOpen.value = true
}

async function onSubmitCreate(): Promise<void> {
  if (creating.value) return
  // 防御性二次校验（UForm 校验之外的最后防线）：不通过则不发起请求
  const residualErrors = validateCreate()
  if (residualErrors.length > 0) {
    createError.value = new ApiError(400, 'bad_request', residualErrors.map(e => e.message).join('；'))
    return
  }
  creating.value = true
  createError.value = null
  try {
    const createdName = createForm.name.trim()
    await createImage({
      name: createdName,
      default_user: createForm.default_user.trim(),
      download_url: createForm.download_url.trim()
    })
    createOpen.value = false
    resetCreateForm()
    toast.add({ title: '创建成功', description: `镜像「${createdName}」已登记`, color: 'success' })
    void load()
  } catch (err) {
    createError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    creating.value = false
  }
}

// ---- 下载操作：目标节点选择（全部节点 / 多选节点），202 异步受理 ----
const downloadTarget = ref<ImageRow | null>(null)
const downloadOpen = computed({
  get: () => downloadTarget.value !== null,
  set: (open: boolean) => {
    if (!open) downloadTarget.value = null
  }
})
const downloadScope = ref<'all' | 'selected'>('all')
const selectedNodeIds = ref<number[]>([])
const downloading = ref(false)
const downloadError = ref<ApiError | null>(null)

function openDownloadModal(row: ImageRow): void {
  downloadTarget.value = row
  downloadScope.value = 'all'
  selectedNodeIds.value = []
  downloadError.value = null
}

async function submitDownload(): Promise<void> {
  const target = downloadTarget.value
  if (!target || downloading.value) return
  if (!target.image.download_url) {
    downloadError.value = new ApiError(400, 'bad_request', '该镜像缺少下载地址，无法发起下载')
    return
  }
  // 目标节点：全部节点 = 已加载 nodes-status 的全部节点；选择节点 = 勾选列表
  const nodeIds = downloadScope.value === 'all'
    ? target.nodes.map(n => n.node_id)
    : selectedNodeIds.value
  if (nodeIds.length === 0) {
    downloadError.value = new ApiError(400, 'bad_request', downloadScope.value === 'all' ? '没有可下载的目标节点' : '请至少选择一个节点')
    return
  }
  downloading.value = true
  downloadError.value = null
  try {
    // 202 受理：后台按节点独立下载，节点状态与操作记录随后台推进
    const res = await downloadImage(target.image.id, { node_ids: nodeIds })
    downloadTarget.value = null
    toast.add({
      title: '下载已受理',
      description: `「${target.image.name}」已在 ${res.data.length} 个节点开始后台下载，可在操作记录中查看进度`,
      color: 'success',
      icon: 'i-lucide-check-circle-2'
    })
    // 刷新列表：节点状态徽标与操作记录随之更新
    void load()
  } catch (err) {
    // 契约错误（含 409 目标节点已有 running 下载记录）由 AppErrorAlert 展示
    downloadError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    downloading.value = false
  }
}

// ---- 操作历史（下载记录，按时间倒序分页） ----
const opsTarget = ref<ImageRow | null>(null)
const opsOpen = computed({
  get: () => opsTarget.value !== null,
  set: (open: boolean) => {
    if (!open) opsTarget.value = null
  }
})
const operations = ref<ImageOperation[]>([])
const opsTotal = ref(0)
const opsLimit = 25 // 契约 QueryLimit 默认值（上限 100）
const opsLoading = ref(false)
const opsLoadingMore = ref(false)
const opsError = ref<ApiError | null>(null)
// 请求序号守卫：弹窗目标切换/刷新时丢弃过期响应
let opsSeq = 0

const OPERATION_RESULT_BADGES = {
  running: { color: 'primary', label: '执行中' },
  success: { color: 'success', label: '成功' },
  failed: { color: 'error', label: '失败' }
} as const

function openOpsModal(row: ImageRow): void {
  opsTarget.value = row
  operations.value = []
  opsTotal.value = 0
  opsError.value = null
  void loadOperations(true)
}

/** 节点名展示：优先行内 nodes（node_id → node_name），其次目录映射，兜底节点 #id */
function nodeLabel(nodeId: number): string {
  const hit = opsTarget.value?.nodes.find(n => n.node_id === nodeId)
  if (hit) return hit.node_name
  return catalogNodeName(nodeId) ?? `节点 #${nodeId}`
}

// 拉取操作记录：first=true 重置分页，否则在末尾追加下一页（接口按时间倒序）
async function loadOperations(first = false): Promise<void> {
  const target = opsTarget.value
  if (!target) return
  const seq = ++opsSeq
  // 每次发起请求前重置错误：避免"加载更多"失败后再次成功时旧错误条残留（对齐 submitCreate/fetchVMs 模式）
  opsError.value = null
  if (first) {
    operations.value = []
    opsLoading.value = true
  } else {
    opsLoadingMore.value = true
  }
  try {
    // 追加模式下以当前已加载条数为 offset，保证页间不重不漏
    const res = await listImageOperations(target.image.id, { limit: opsLimit, offset: operations.value.length })
    // 过期响应丢弃：期间弹窗已切换目标或重新加载
    if (seq !== opsSeq) return
    operations.value = [...operations.value, ...res.data]
    opsTotal.value = res.total
  } catch (err) {
    if (seq !== opsSeq) return
    opsError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (seq === opsSeq) {
      opsLoading.value = false
      opsLoadingMore.value = false
    }
  }
}

// 是否还有更多记录：已加载条数小于服务端总数
const hasMoreOps = computed(() => operations.value.length < opsTotal.value)

// 表格列定义（accessorKey 仅作列 id，所有列均由插槽渲染 row.original.image 字段）
const columns = [
  { accessorKey: 'name', header: '镜像名' },
  { accessorKey: 'default_user', header: '默认用户' },
  { accessorKey: 'download_url', header: '下载地址' },
  { accessorKey: 'created_at', header: '创建时间' },
  { accessorKey: 'node_status', header: '节点状态' },
  { accessorKey: 'actions', header: '操作' }
]
</script>

<template>
  <UDashboardPanel>
    <template #header>
      <UDashboardNavbar title="镜像">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #trailing>
          <UButton
            icon="i-lucide-plus"
            color="primary"
            @click="openCreateModal"
          >
            登记镜像
          </UButton>
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <div class="space-y-4 p-4">
        <AppErrorAlert
          v-if="error"
          :code="error.code"
          :message="error.message"
          title="镜像列表加载失败"
        />

        <AppLoading
          v-else-if="loading && rows.length === 0"
          :rows="4"
        />

        <template v-else>
          <!-- 统计 + 工具栏：区域过滤 + 手动刷新（节点状态为实时扫描，刷新即时重扫） -->
          <div class="flex flex-wrap items-center justify-between gap-3 px-1">
            <p class="text-sm text-muted">
              共 {{ total }} 个镜像（下载状态为各节点 local/import 目录实时扫描结果）
            </p>
            <div class="flex items-center gap-2">
              <USelect
                :model-value="zoneFilter ?? 0"
                :items="zoneOptions"
                class="w-44"
                aria-label="按区域过滤镜像"
                @update:model-value="onZoneFilterChange"
              />
              <UButton
                icon="i-lucide-refresh-cw"
                variant="outline"
                :loading="loading"
                @click="load"
              >
                刷新
              </UButton>
            </div>
          </div>

          <UCard
            v-if="rows.length > 0"
            :ui="{ body: 'p-0' }"
          >
            <UTable
              :data="rows"
              :columns="columns"
            >
              <template #name-cell="{ row }">
                <span class="font-medium">{{ row.original.image.name }}</span>
              </template>

              <template #default_user-cell="{ row }">
                <code class="text-sm">{{ row.original.image.default_user }}</code>
              </template>

              <!-- 下载地址：长 URL 截断展示，title 显示全文；旧数据缺地址降级为 — -->
              <template #download_url-cell="{ row }">
                <span
                  v-if="row.original.image.download_url"
                  class="block max-w-52 truncate font-mono text-xs"
                  :title="row.original.image.download_url"
                >
                  {{ row.original.image.download_url }}
                </span>
                <span
                  v-else
                  class="text-sm text-muted"
                >
                  —
                </span>
              </template>

              <template #created_at-cell="{ row }">
                <span class="text-sm text-muted">{{ formatDateTime(row.original.image.created_at) }}</span>
              </template>

              <!-- 节点状态：行挂载时并行扫描全部启用节点；加载中 spinner，失败可重试 -->
              <template #node_status-cell="{ row }">
                <div class="flex flex-wrap items-center gap-1">
                  <template v-if="row.original.loading">
                    <UIcon
                      name="i-lucide-loader-circle"
                      class="size-4 animate-spin text-muted"
                    />
                    <span class="text-xs text-muted">扫描中…</span>
                  </template>

                  <template v-else-if="row.original.error">
                    <UBadge
                      label="状态加载失败"
                      color="error"
                      variant="subtle"
                      :title="row.original.error.message"
                    />
                    <UButton
                      size="xs"
                      variant="ghost"
                      color="neutral"
                      icon="i-lucide-refresh-cw"
                      aria-label="重试加载节点状态"
                      @click="loadRowStatus(row.original, loadGen)"
                    />
                  </template>

                  <span
                    v-else-if="row.original.nodes.length === 0"
                    class="text-sm text-muted"
                  >
                    无启用节点
                  </span>

                  <template v-else>
                    <!-- 前 8 个节点直接展示；节点多时其余折叠（UCollapsible 现有模式） -->
                    <template
                      v-for="node in row.original.nodes.slice(0, 8)"
                      :key="node.node_id"
                    >
                      <UBadge
                        :color="node.downloaded ? 'success' : 'neutral'"
                        variant="subtle"
                        :label="nodeBadgeLabel(node)"
                        :title="nodeBadgeTitle(node)"
                      />
                    </template>
                    <UCollapsible v-if="row.original.nodes.length > 8">
                      <template #trigger="{ open }">
                        <UButton
                          size="xs"
                          variant="ghost"
                          color="neutral"
                          :icon="open ? 'i-lucide-chevron-up' : 'i-lucide-chevron-down'"
                          :label="open ? '收起' : `其余 ${row.original.nodes.length - 8} 个节点`"
                        />
                      </template>
                      <div class="mt-1 flex flex-wrap gap-1">
                        <UBadge
                          v-for="node in row.original.nodes.slice(8)"
                          :key="node.node_id"
                          :color="node.downloaded ? 'success' : 'neutral'"
                          variant="subtle"
                          :label="nodeBadgeLabel(node)"
                          :title="nodeBadgeTitle(node)"
                        />
                      </div>
                    </UCollapsible>
                  </template>
                </div>
              </template>

              <!-- 操作：下载（节点状态加载中/无目标节点/缺下载地址时禁用）+ 操作历史 -->
              <template #actions-cell="{ row }">
                <div class="flex items-center gap-1">
                  <UButton
                    size="sm"
                    variant="ghost"
                    color="primary"
                    icon="i-lucide-download"
                    :disabled="row.original.loading || row.original.nodes.length === 0 || !row.original.image.download_url"
                    :title="!row.original.image.download_url ? '该镜像缺少下载地址，无法下载' : undefined"
                    @click="openDownloadModal(row.original)"
                  >
                    下载
                  </UButton>
                  <UButton
                    size="sm"
                    variant="ghost"
                    icon="i-lucide-history"
                    @click="openOpsModal(row.original)"
                  >
                    记录
                  </UButton>
                </div>
              </template>
            </UTable>
          </UCard>

          <AppEmpty
            v-else
            title="暂无镜像"
            description="点击右上角「登记镜像」录入镜像名与下载地址，再在各节点发起下载。"
            icon="i-lucide-image"
          />
        </template>
      </div>

      <!-- BUG-1：UModal 默认插槽是 DialogTrigger（as-child 内联渲染），内容放默认插槽会内联铺在页面上、
           点击提交还会连带触发打开空弹窗；因此弹窗内容必须进 #body、按钮进 #footer。
           UModal 位于 panel 的 #body 具名插槽末尾（panel 默认插槽为空，不触发插槽冲突）；
           且 UModal 默认 portal 渲染到 body，DOM 位置不影响表现，弹窗只在点击按钮后打开。
           lint 约束：vue/no-multiple-template-root 禁止多根，UModal 无法作为 panel 的兄弟根。 -->
      <!-- 登记镜像弹窗：name / default_user / download_url（http(s) 必填） -->
      <UModal
        v-model:open="createOpen"
        title="登记镜像"
        description="登记后可在任意节点发起后台下载（下载地址域名需在服务端白名单内）"
        :dismissible="!creating"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <UForm
            ref="createFormRef"
            :validate="validateCreate"
            :validate-on="['blur', 'change']"
            class="space-y-4"
            @submit="onSubmitCreate"
          >
            <UFormField
              name="name"
              label="镜像名"
              required
              :hint="'全局唯一，如 debian-12-cloud'"
            >
              <UInput
                v-model="createForm.name"
                placeholder="如 debian-12-cloud"
              />
            </UFormField>

            <UFormField
              name="default_user"
              label="默认登录用户"
              required
              :hint="'镜像内置的默认用户，如 debian'"
            >
              <UInput
                v-model="createForm.default_user"
                placeholder="如 debian"
              />
            </UFormField>

            <UFormField
              name="download_url"
              label="下载地址"
              required
              :hint="'http/https 地址，镜像文件由目标 PVE 节点代发下载'"
            >
              <UInput
                v-model="createForm.download_url"
                type="url"
                placeholder="如 https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
                autocomplete="off"
              />
            </UFormField>

            <AppErrorAlert
              v-if="createError"
              :code="createError.code"
              :message="createError.message"
              title="创建失败"
            />
          </UForm>
        </template>

        <template #footer>
          <UButton
            label="取消"
            variant="outline"
            :disabled="creating"
            @click="createOpen = false"
          />
          <UButton
            label="登记"
            color="primary"
            icon="i-lucide-plus"
            :loading="creating"
            @click="createFormRef?.submit()"
          />
        </template>
      </UModal>

      <!-- 下载弹窗：目标节点选择（全部节点 radio / 多选节点 checkbox），202 异步受理 -->
      <UModal
        v-model:open="downloadOpen"
        :title="downloadTarget ? `下载镜像「${downloadTarget.image.name}」` : '下载镜像'"
        :description="downloadTarget?.image.download_url
          ? '后台按节点独立下载，进度可在操作记录中查看；已有执行中下载的节点会被拒绝（409）'
          : undefined"
        :dismissible="!downloading"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <div class="space-y-4">
            <template v-if="downloadTarget">
              <div class="rounded-md border border-dashed border-muted px-3 py-2 text-sm">
                <div class="flex flex-wrap gap-x-6 gap-y-1">
                  <span>
                    <span class="text-muted">镜像：</span>
                    {{ downloadTarget.image.name }}
                  </span>
                  <span>
                    <span class="text-muted">节点：</span>
                    {{ downloadTarget.nodes.length }} 个启用节点
                  </span>
                </div>
              </div>

              <div class="space-y-2">
                <!-- 目标范围选择：URadioGroup（v4 无单独 URadio，items prop 承载选项） -->
                <URadioGroup
                  v-model="downloadScope"
                  :items="[{
                    value: 'all',
                    label: `全部节点（${downloadTarget.nodes.length} 个）`
                  }, {
                    value: 'selected',
                    label: `选择节点（已选 ${selectedNodeIds.length} 个）`
                  }]"
                />
              </div>

              <!-- 多选节点列表：数据复用已加载 nodes-status 的 node_id/node_name -->
              <div
                v-if="downloadScope === 'selected'"
                class="max-h-56 overflow-y-auto rounded-md border border-muted p-2"
              >
                <!-- BUG-4 修复（同 ip-pools.vue）：单独 UCheckbox 是 reka-ui 单值模式，
                     v-model 数组会被覆盖为布尔值；必须经 UCheckboxGroup（items prop）承载多选 -->
                <UCheckboxGroup
                  v-model="selectedNodeIds"
                  value-key="id"
                  :items="downloadTarget.nodes.map(n => ({
                    id: n.node_id,
                    label: n.node_name,
                    description: n.downloaded ? '该节点已存在该镜像' : '尚未下载'
                  }))"
                />
                <p
                  v-if="downloadTarget.nodes.length === 0"
                  class="px-2 py-1 text-sm text-muted"
                >
                  无启用节点可下载
                </p>
              </div>
            </template>

            <AppErrorAlert
              v-if="downloadError"
              :code="downloadError.code"
              :message="downloadError.message"
              title="下载受理失败"
            />
          </div>
        </template>

        <template #footer>
          <UButton
            variant="outline"
            :disabled="downloading"
            @click="downloadTarget = null"
          >
            取消
          </UButton>
          <UButton
            color="primary"
            icon="i-lucide-download"
            :loading="downloading"
            :disabled="downloadScope === 'selected' && selectedNodeIds.length === 0"
            @click="submitDownload"
          >
            开始下载
          </UButton>
        </template>
      </UModal>

      <!-- 操作历史弹窗：下载记录按时间倒序，running 蓝 / success 绿 / failed 红，
           失败原因（error_message）截断展示、title 全文 -->
      <UModal
        v-model:open="opsOpen"
        :title="opsTarget ? `操作记录（${opsTarget.image.name}）` : '操作记录'"
        :description="opsTotal > 0 ? `共 ${opsTotal} 条，按时间倒序` : undefined"
      >
        <template #body>
          <div class="space-y-3">
            <AppLoading
              v-if="opsLoading"
              :rows="5"
            />

            <template v-else>
              <AppErrorAlert
                v-if="opsError"
                :code="opsError.code"
                :message="opsError.message"
                title="操作记录加载失败"
              />

              <AppEmpty
                v-else-if="operations.length === 0"
                title="暂无操作记录"
                description="对该镜像发起的下载操作将记录于此（每次每节点一条）。"
                icon="i-lucide-history"
              />

              <div
                v-else
                class="space-y-2"
              >
                <div
                  v-for="op in operations"
                  :key="op.id"
                  class="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 rounded-md border border-muted px-3 py-2 text-sm"
                >
                  <div class="flex items-center gap-2">
                    <UBadge
                      :color="OPERATION_RESULT_BADGES[op.result].color"
                      variant="soft"
                      :label="OPERATION_RESULT_BADGES[op.result].label"
                    />
                    <span class="font-medium">下载</span>
                    <span class="text-muted">节点 {{ nodeLabel(op.node_id) }}</span>
                  </div>
                  <div class="flex items-center gap-2">
                    <span
                      v-if="op.error_message"
                      class="max-w-56 truncate text-xs text-error"
                      :title="op.error_message"
                    >
                      {{ op.error_message }}
                    </span>
                    <span class="text-xs text-muted">{{ formatDateTime(op.created_at) }}</span>
                  </div>
                </div>

                <!-- 加载更多：倒序分页追加（limit 25） -->
                <UButton
                  v-if="hasMoreOps"
                  block
                  variant="outline"
                  :loading="opsLoadingMore"
                  @click="loadOperations(false)"
                >
                  加载更多
                </UButton>
              </div>
            </template>
          </div>
        </template>

        <template #footer>
          <UButton
            variant="outline"
            @click="opsTarget = null"
          >
            关闭
          </UButton>
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
