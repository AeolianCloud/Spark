<script setup lang="ts">
// 存储类型管理（扫描同步形态）：列表（区域过滤 + limit/offset 分页 + X-Total-Count 总数）
// + 手动触发扫描（按 zone，展示 created/updated/deleted/skipped 摘要）+ 编辑业务名
// + enabled 开关。存储类型由扫描从 PVE 自动同步产生（不再支持手动登记），
// pve_storage 为扫描权威字段不可手改；展示语义 name || pve_storage。
import {
  listStorageTypes,
  scanStorageTypes,
  updateStorageType
} from '~/api/storage-types'
import { listZones } from '~/api/zones'
import { ApiError } from '~/api/client'
import type { StorageScanSummary, StorageType, StorageTypeCapabilities, ZoneResponse } from '~/api/types'

const toast = useToast()

// 列表状态：分页驱动（limit/offset，每页条数取契约默认 25）
const PAGE_LIMIT = 25
const loading = ref(true)
const error = ref<ApiError | null>(null)
const items = ref<StorageType[]>([])
const total = ref(0)
const page = ref(1)

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / PAGE_LIMIT)))

// 请求序号守卫：快速翻页/切区域时丢弃过期响应（慢响应不覆盖快响应）
let loadSeq = 0

async function load(): Promise<void> {
  const seq = ++loadSeq
  loading.value = true
  error.value = null
  try {
    const res = await listStorageTypes({
      limit: PAGE_LIMIT,
      offset: (page.value - 1) * PAGE_LIMIT,
      zone_id: zoneFilter.value
    })
    // 已发起更新的请求：本响应过期，丢弃
    if (seq !== loadSeq) return
    items.value = res.data
    total.value = res.total
  } catch (err) {
    if (seq !== loadSeq) return
    error.value = err as ApiError
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

// ---- 区域（过滤 + 扫描目标）----
const zones = ref<ZoneResponse[]>([])

// 区域过滤（可选能力）：undefined = 全部区域
const zoneFilter = ref<number | undefined>(undefined)
// USelect 以 0 代表"全部区域"（区域 id 恒为正整数，0 不冲突）
const zoneOptions = computed(() => [
  { label: '全部区域', value: 0 },
  ...zones.value.map(z => ({ label: z.name, value: z.id }))
])

function onZoneFilterChange(value: number | undefined): void {
  zoneFilter.value = value === undefined || value === 0 ? undefined : value
}

// 扫描弹窗区域选项（扫描必须指定具体区域，不含"全部区域"）
const scanZoneOptions = computed(() => zones.value.map(z => ({ label: z.name, value: z.id })))

/** zone_id → 区域名映射（找不到时回退显示 ID） */
function zoneNameOf(zoneId: number): string {
  return zones.value.find(z => z.id === zoneId)?.name ?? `#${zoneId}`
}

async function loadZones(): Promise<void> {
  try {
    zones.value = (await listZones({ limit: 100 })).data
  } catch {
    // 区域列表加载失败不阻塞列表：仅"全部区域"可用，扫描弹窗内无法选择区域
    zones.value = []
  }
}

onMounted(() => {
  void load()
  void loadZones()
})

// 页码变化时重新拉取对应分页数据
watch(page, () => {
  void load()
})

// 区域过滤变化：重置到第一页并重新加载（仅一次请求：page 已为 1 时由本 watch 直接加载）
watch(zoneFilter, () => {
  if (page.value === 1) {
    void load()
  } else {
    page.value = 1
  }
})

// ---- 展示辅助 ----
/** 展示语义：name || pve_storage（业务名为空时回退到 PVE 存储名） */
function displayNameOf(item: StorageType): string {
  return item.name ?? item.pve_storage
}

// 能力标签：按 capabilities 布尔派生（PVE 全部内容枚举 + 云镜像下载）
const CAPABILITY_TAGS: { key: keyof StorageTypeCapabilities, label: string }[] = [
  { key: 'can_store_images', label: '磁盘映像' },
  { key: 'can_store_iso', label: 'ISO' },
  { key: 'can_store_backup', label: '备份' },
  { key: 'can_store_vztmpl', label: '容器模板' },
  { key: 'can_store_rootdir', label: '容器根目录' },
  { key: 'can_store_snippets', label: '脚本片段' },
  { key: 'can_download_image', label: '云镜像下载' }
]

/** 该存储具备的能力标签列表（按 capabilities 布尔过滤） */
function capabilityTagsOf(cap: StorageTypeCapabilities): { key: keyof StorageTypeCapabilities, label: string }[] {
  return CAPABILITY_TAGS.filter(t => cap[t.key])
}

// ---- 按节点分组展示 ----
// 数据源：storage types 列表（含 nodes 挂载快照）+ zones 列表（ZoneResponse 已含该区域
// 全部节点 nodes 数组，无需单独调节点接口）。分组随 zoneFilter 联动，两种模式：
//   1. 选中具体区域：按该区域的节点分组，组标题为节点名（如 "pve1"）；
//   2. "全部区域"：遍历所有 zones 的节点生成分组，组标题为 "区域名 / 节点名"
//      （如 "zoneA / pve1"，防跨 zone 同名节点混淆）。分组时校验存储的 zone 归属
//      （s.zone_id === zone.id），否则 zoneB 的 "pve1" 组会把 zoneA 挂载 pve1 的
//      存储也列进去。
// 匹配键：存储 nodes 存 PVE 集群节点名，与节点登记的 pve_name 对应（pve_name 为空时
// 业务名即集群名，取 pve_name || name 匹配）。
// 多节点挂载的存储在其每个挂载节点组下都列出（与该存储在该节点可用的调度语义一致）。
// 分页取舍：仍按存储 limit/offset 分页，分组只是当前页数据的展示层重组（不引入跨页分组
// 的复杂度）；某节点组的存储被分页挤出时该组临时消失属预期，翻页后恢复。
// 空态取舍：无挂载存储的节点组直接隐藏——节点自身状态由节点管理页承载，避免大量空组占位噪音。
// 兜底取舍：zones 加载失败或全部 zone 无节点时退回平铺，保证数据可见。
interface StorageGroup {
  key: string
  title: string
  items: StorageType[]
}

/** zone + 节点名 的扁平条目（pve_name 为空时回退业务名即集群名） */
interface NodeEntry {
  zoneId: number
  nodeName: string
}

/** 由 zones 列表构建节点条目列表（zones 加载失败时为空） */
function nodeEntriesOf(zonesList: ZoneResponse[]): NodeEntry[] {
  const entries: NodeEntry[] = []
  for (const zone of zonesList) {
    for (const node of zone.nodes ?? []) {
      entries.push({ zoneId: zone.id, nodeName: node.pve_name || node.name })
    }
  }
  return entries
}

const groups = computed<StorageGroup[]>(() => {
  // 兜底：列表有数据但分组为空（无节点上下文）时退回平铺，保证数据可见
  const fallback: StorageGroup[] = [{ key: 'all', title: '所有节点', items: items.value }]

  const isAllZones = zoneFilter.value === undefined
  // 节点条目：选中具体区域时取该区域；"全部区域"时取所有 zones（zones 加载失败时为空）
  const entries = isAllZones
    ? nodeEntriesOf(zones.value)
    : nodeEntriesOf(zones.value.filter(z => z.id === zoneFilter.value))
  if (entries.length === 0) return fallback

  // 全部已登记节点名集合（用于"所有节点"组的未匹配判定）
  const allNodeNames = entries.map(e => e.nodeName)

  const result: StorageGroup[] = []
  // 每个节点一组：列出挂载该节点名且 zone 归属相符的存储。key 带 zoneId 前缀，
  // 防"全部区域"模式下不同 zone 的同名节点（zoneA/pve1 与 zoneB/pve1）key 冲突
  for (const entry of entries) {
    const nodeItems = items.value.filter(s => s.zone_id === entry.zoneId && s.nodes.includes(entry.nodeName))
    if (nodeItems.length > 0) {
      result.push({
        key: isAllZones ? `zone-${entry.zoneId}-node-${entry.nodeName}` : `node-${entry.nodeName}`,
        title: isAllZones ? `${zoneNameOf(entry.zoneId)} / ${entry.nodeName}` : entry.nodeName,
        items: nodeItems
      })
    }
  }
  // "所有节点"组：nodes 为空（不限制节点、所有节点可用）的存储。
  // 兜底语义：nodes 非空但与已登记节点名（allNodeNames，全部区域模式下为全部 zone 的
  // 节点名集合）无任何交集的存储（如挂载在未登记节点、或 pve_name 未回填匹配不上）
  // 既进不了节点组，也归入本组——与"无节点上下文时退回平铺"同一兜底，保证数据不因
  // 匹配不上而从页面消失（!some 恒包含空数组情形；跨 zone 同名节点属正常匹配范围）
  const unboundItems = items.value.filter(s => !s.nodes.some(n => allNodeNames.includes(n)))
  if (unboundItems.length > 0) {
    result.push({ key: 'all', title: '所有节点', items: unboundItems })
  }
  return result.length > 0 ? result : fallback
})

// 组内表格列定义（各组共用，accessorKey 与 cell 插槽名对应）
const tableColumns = [{
  accessorKey: 'name',
  header: '名称'
}, {
  accessorKey: 'zone_id',
  header: '区域'
}, {
  accessorKey: 'type',
  header: '类型'
}, {
  accessorKey: 'capabilities',
  header: '能力'
}, {
  accessorKey: 'enabled',
  header: '启用'
}, {
  accessorKey: 'created_at',
  header: '创建时间'
}, {
  accessorKey: 'actions',
  header: '操作'
}]

// ---- 扫描：手动触发指定区域扫描 + 摘要展示 ----
const scanOpen = ref(false)
const scanning = ref(false)
const scanZoneId = ref<number | undefined>(undefined)
const scanError = ref<ApiError | null>(null)
const scanSummary = ref<StorageScanSummary | null>(null)

function openScanModal(): void {
  scanZoneId.value = undefined
  scanError.value = null
  scanSummary.value = null
  scanOpen.value = true
}

async function onScan(): Promise<void> {
  if (scanZoneId.value === undefined) return
  scanning.value = true
  scanError.value = null
  scanSummary.value = null
  try {
    const res = await scanStorageTypes(scanZoneId.value)
    scanSummary.value = res.data
    toast.add({
      title: '扫描完成',
      description: `新建 ${res.data.created} 个 · 更新 ${res.data.updated} 个 · 删除 ${res.data.deleted} 个 · 跳过 ${res.data.skipped} 个`,
      color: 'success',
      icon: 'i-lucide-scan-search'
    })
    await load()
  } catch (err) {
    scanError.value = err as ApiError
  } finally {
    scanning.value = false
  }
}

// ---- enabled 开关：行内切换（失败回滚并提示）----
async function onToggleEnabled(item: StorageType, enabled: boolean): Promise<void> {
  try {
    await updateStorageType(item.id, { enabled })
    item.enabled = enabled
    toast.add({
      title: '已更新',
      description: `存储「${displayNameOf(item)}」已${enabled ? '启用' : '禁用'}`,
      color: 'success'
    })
  } catch (err) {
    toast.add({ title: '保存失败', description: err instanceof ApiError ? err.message : '未知错误', color: 'error' })
  }
}

// ---- 编辑业务名（可空/可置空）----
const editOpen = ref(false)
const savingName = ref(false)
const editError = ref<ApiError | null>(null)
const editingItem = ref<StorageType | null>(null)
const editName = ref('')

function openEditModal(item: StorageType): void {
  editingItem.value = item
  editName.value = item.name ?? ''
  editError.value = null
  editOpen.value = true
}

async function onSaveName(): Promise<void> {
  if (!editingItem.value) return
  savingName.value = true
  editError.value = null
  try {
    // 空串表示置空为 NULL（展示回退到 pve_storage）
    await updateStorageType(editingItem.value.id, { name: editName.value.trim() })
    toast.add({
      title: '保存成功',
      description: `存储「${editingItem.value.pve_storage}」的业务名已更新`,
      color: 'success'
    })
    editOpen.value = false
    await load()
  } catch (err) {
    editError.value = err as ApiError
  } finally {
    savingName.value = false
  }
}
</script>

<template>
  <UDashboardPanel>
    <template #header>
      <UDashboardNavbar title="存储类型">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #trailing>
          <UButton
            icon="i-lucide-scan-search"
            color="primary"
            @click="openScanModal"
          >
            扫描存储
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
          title="存储类型列表加载失败"
        />

        <AppLoading
          v-else-if="loading"
          :rows="4"
        />

        <template v-else>
          <!-- 工具栏：统计 + 区域过滤（扫描按钮在顶栏） -->
          <div class="flex flex-wrap items-center justify-between gap-3 px-1">
            <p class="text-sm text-muted">
              共 {{ total }} 条 · 第 {{ page }}/{{ totalPages }} 页
            </p>
            <USelect
              :model-value="zoneFilter ?? 0"
              :items="zoneOptions"
              class="w-44"
              aria-label="按区域过滤存储"
              @update:model-value="onZoneFilterChange"
            />
          </div>

          <!-- 按节点分组列表：每组一个卡片（节点组 + "所有节点"组），组内表格列共用 -->
          <template v-if="items.length > 0">
            <div
              v-for="group in groups"
              :key="group.key"
              class="space-y-2"
            >
              <!-- 组头：具体区域模式显示节点名（如 pve1）；全部区域模式显示"区域名 / 节点名"（如 zoneA / pve1）；"所有节点"组不变 -->
              <div class="flex items-center gap-2 px-1">
                <span class="font-medium">{{ group.title }}</span>
                <UBadge
                  size="sm"
                  variant="soft"
                  color="neutral"
                  :label="`${group.items.length} 个`"
                />
              </div>

              <UCard
                :ui="{ body: 'p-0' }"
              >
                <UTable
                  :data="group.items"
                  :columns="tableColumns"
                >
                  <!-- 名称：业务名优先，为空回退到 PVE 存储名（title 提示）；设置了业务名时副行展示 PVE 存储名 -->
                  <template #name-cell="{ row }">
                    <div class="min-w-40">
                      <span
                        class="font-medium"
                        :title="row.original.name === null ? '未设置业务名，展示 PVE 存储名' : undefined"
                      >
                        {{ displayNameOf(row.original) }}
                      </span>
                      <p
                        v-if="row.original.name !== null"
                        class="mt-0.5 text-xs text-muted"
                      >
                        {{ row.original.pve_storage }}
                      </p>
                    </div>
                  </template>

                  <template #zone_id-cell="{ row }">
                    <span class="text-sm">{{ zoneNameOf(row.original.zone_id) }}</span>
                  </template>

                  <template #type-cell="{ row }">
                    <code class="text-sm">{{ row.original.type ?? '—' }}</code>
                  </template>

                  <!-- 能力标签：按 capabilities 布尔派生（磁盘映像/ISO/备份等） -->
                  <template #capabilities-cell="{ row }">
                    <div class="flex max-w-72 flex-wrap gap-1">
                      <UBadge
                        v-for="tag in capabilityTagsOf(row.original.capabilities)"
                        :key="tag.key"
                        size="sm"
                        variant="soft"
                        color="neutral"
                        :label="tag.label"
                      />
                      <span
                        v-if="capabilityTagsOf(row.original.capabilities).length === 0"
                        class="text-sm text-muted"
                      >
                        —
                      </span>
                    </div>
                  </template>

                  <!-- 启用开关：行内切换，失败回滚（后端拒绝不可用的存储被虚拟机选用） -->
                  <template #enabled-cell="{ row }">
                    <USwitch
                      :model-value="row.original.enabled"
                      size="sm"
                      :aria-label="`${displayNameOf(row.original)} 启用开关`"
                      @update:model-value="(v: boolean) => onToggleEnabled(row.original, v)"
                    />
                  </template>

                  <template #created_at-cell="{ row }">
                    <span class="text-sm text-muted">{{ formatDateTime(row.original.created_at) }}</span>
                  </template>

                  <template #actions-cell="{ row }">
                    <UButton
                      icon="i-lucide-pencil"
                      label="编辑名称"
                      color="neutral"
                      variant="ghost"
                      size="sm"
                      @click="openEditModal(row.original)"
                    />
                  </template>
                </UTable>
              </UCard>
            </div>
          </template>

          <AppEmpty
            v-else
            title="暂无存储类型"
            description="点击右上角「扫描存储」，从所选区域的 PVE 集群自动同步存储。"
            icon="i-lucide-hard-drive"
          />

          <div
            v-if="totalPages > 1"
            class="flex justify-center"
          >
            <UPagination
              v-model:page="page"
              :items-per-page="PAGE_LIMIT"
              :total="total"
            />
          </div>
        </template>
      </div>

      <!-- BUG-1：UModal 默认插槽是 DialogTrigger（as-child 内联渲染），内容放默认插槽会内联铺在页面上、
           点击提交还会连带触发打开空弹窗；因此弹窗内容必须进 #body、按钮进 #footer。
           UModal 位于 panel 的 #body 具名插槽末尾（panel 默认插槽为空，不触发插槽冲突）；
           且 UModal 默认 portal 渲染到 body，DOM 位置不影响表现，弹窗只在点击按钮后打开。
           lint 约束：vue/no-multiple-template-root 禁止多根，UModal 无法作为 panel 的兄弟根。 -->
      <UModal
        v-model:open="scanOpen"
        title="扫描存储"
        :description="'从所选区域的 PVE 集群自动发现存储并同步本地；PVE 已消失且未被虚拟机引用的存储会被删除。'"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <div class="space-y-4">
            <UFormField
              name="scan_zone"
              label="扫描区域"
              required
              :hint="'存储按区域隔离，一个区域对应一个 PVE 集群'"
            >
              <USelect
                v-model="scanZoneId"
                :items="scanZoneOptions"
                placeholder="请选择区域"
              />
            </UFormField>

            <UAlert
              v-if="scanSummary"
              color="success"
              variant="subtle"
              icon="i-lucide-scan-search"
              :title="'扫描完成'"
              :description="`新建 ${scanSummary.created} 个 · 更新 ${scanSummary.updated} 个 · 删除 ${scanSummary.deleted} 个 · 跳过 ${scanSummary.skipped} 个（跳过：被虚拟机引用而无法删除）`"
            />

            <AppErrorAlert
              v-if="scanError"
              :code="scanError.code"
              :message="scanError.message"
              title="扫描失败"
            />
          </div>
        </template>

        <template #footer>
          <UButton
            label="取消"
            variant="outline"
            @click="scanOpen = false"
          />
          <UButton
            label="开始扫描"
            color="primary"
            icon="i-lucide-scan-search"
            :loading="scanning"
            :disabled="scanZoneId === undefined"
            @click="onScan"
          />
        </template>
      </UModal>

      <UModal
        v-model:open="editOpen"
        :title="editingItem ? `编辑业务名：${editingItem.pve_storage}` : '编辑业务名'"
        :description="'业务名可留空：留空表示置空，展示回退到 PVE 存储名。'"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <div class="space-y-4">
            <UFormField
              name="name"
              label="业务名"
              :hint="'留空 = 置空，展示回退到 PVE 存储名（如 固态硬盘）'"
            >
              <UInput
                v-model="editName"
                placeholder="如 固态硬盘"
              />
            </UFormField>

            <AppErrorAlert
              v-if="editError"
              :code="editError.code"
              :message="editError.message"
              title="保存失败"
            />
          </div>
        </template>

        <template #footer>
          <UButton
            label="取消"
            variant="outline"
            @click="editOpen = false"
          />
          <UButton
            label="保存"
            color="primary"
            icon="i-lucide-check"
            :loading="savingName"
            @click="onSaveName"
          />
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
