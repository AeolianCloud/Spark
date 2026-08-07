<script setup lang="ts">
// 节点总览：跨可用区浏览全部节点。
// 设计：listZones 响应内联各区域的完整节点列表（契约保证 nodes 恒为数组），
// 因此仅需一次列表调用即可展开全部节点，无需对每个 Zone 扇出节点查询。
// 实时状态：rows 就绪后对每个节点并行 GET /nodes/:id/status（Promise.all），
// 单节点失败（PVE 不可达 503 node_unavailable 等）不影响其他节点，该行展示"不可达"徽标。
import { listZones } from '~/api/zones'
import { getNodeStatus } from '~/api/nodes'
import { ApiError } from '~/api/client'
import type { NodeResponse, NodeStatusResponse, ZoneResponse } from '~/api/types'
import { formatPercent } from '~/utils/format'

const loading = ref(true)
const error = ref<ApiError | null>(null)
const zones = ref<ZoneResponse[]>([])

// 节点 id → 实时状态条目：data（可达）与 error（不可达 503 等）互斥
interface NodeStatusEntry {
  /** 状态响应数据（节点可达） */
  data?: NodeStatusResponse
  /** 请求失败错误（PVE 不可达等） */
  error?: ApiError
}
const statusMap = ref<Map<number, NodeStatusEntry>>(new Map())

// 请求序号守卫（最后意图胜出，与 vms.vue 的 fetchSeq 模式一致）：每次发起 ++fetchSeq，
// 响应到达时序号不匹配即过期丢弃，防止刷新在途时再次点击刷新，旧响应覆盖新数据
let fetchSeq = 0

async function load(): Promise<void> {
  const seq = ++fetchSeq
  loading.value = true
  error.value = null
  try {
    // 分页上限 100：可用区超过 100 个时按第一页口径统计
    const res = await listZones({ limit: 100 })
    // 过期响应丢弃：期间已发起更新的请求（如再次刷新），本响应不落盘
    if (seq !== fetchSeq) return
    zones.value = res.data
    await loadStatuses(seq)
    // 过期响应丢弃：状态落盘前再校验一次（并行请求状态期间可能又发起新刷新）
    if (seq !== fetchSeq) return
  } catch (err) {
    if (seq !== fetchSeq) return
    // 刷新失败保留旧数据（仅展示错误条）；首次加载无数据时自然展示错误态
    error.value = err as ApiError
  } finally {
    if (seq === fetchSeq) loading.value = false
  }
}

/** 并行拉取全部节点的实时状态；单节点失败仅记录 error，不阻塞其余请求 */
async function loadStatuses(seq: number): Promise<void> {
  const entries = await Promise.all(rows.value.map(async ({ node }) => {
    try {
      const res = await getNodeStatus(node.id)
      return { id: node.id, entry: { data: res.data } }
    } catch (err) {
      const e = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
      return { id: node.id, entry: { error: e } }
    }
  }))
  // 过期响应丢弃：期间已发起更新的请求（如再次刷新），本响应不落盘
  if (seq !== fetchSeq) return
  statusMap.value = new Map(entries.map(e => [e.id, e.entry]))
}

/** 节点实时状态条目（不存在返回 undefined：状态仍在加载中） */
function statusEntry(nodeId: number): NodeStatusEntry | undefined {
  return statusMap.value.get(nodeId)
}

onMounted(load)

/** 全部节点平铺行：zone + node 组合，供跨区域表格展示 */
const rows = computed<Array<{ zone: ZoneResponse, node: NodeResponse }>>(() =>
  zones.value.flatMap(zone => zone.nodes.map(node => ({ zone, node })))
)

/** 汇总统计：区域数与节点总数（第一页口径） */
const zoneCount = computed(() => zones.value.length)
const nodeCount = computed(() => rows.value.length)
const enabledCount = computed(() => rows.value.filter(({ node }) => node.enabled).length)
</script>

<template>
  <UDashboardPanel>
    <template #header>
      <UDashboardNavbar title="节点">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #trailing>
          <UButton
            icon="i-lucide-refresh-cw"
            variant="ghost"
            :loading="loading"
            @click="load"
          >
            刷新
          </UButton>
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <div class="space-y-4 p-4">
        <!-- 列表加载失败（含刷新失败）：独立展示，不阻塞下方旧表格 -->
        <AppErrorAlert
          v-if="error"
          :code="error.code"
          :message="error.message"
          title="节点列表加载失败"
        />

        <!-- 加载态骨架屏：仅首次加载（无数据）时显示；刷新保留旧表格，避免闪烁 -->
        <AppLoading
          v-if="rows.length === 0 && loading"
          :rows="4"
        />

        <template v-else>
          <div class="flex flex-wrap items-center gap-2 px-1">
            <UBadge
              :label="`${zoneCount} 个可用区`"
              color="neutral"
              variant="subtle"
            />
            <UBadge
              :label="`${nodeCount} 个节点`"
              color="neutral"
              variant="subtle"
            />
            <UBadge
              :label="`${enabledCount} 个启用`"
              color="success"
              variant="subtle"
            />
          </div>

          <UCard
            v-if="rows.length > 0"
            :ui="{ body: 'p-0' }"
          >
            <UTable
              :data="rows"
              :columns="[{
                accessorKey: 'zone',
                header: '可用区'
              }, {
                accessorKey: 'node',
                header: '节点'
              }, {
                accessorKey: 'host',
                header: '主机'
              }, {
                accessorKey: 'enabled',
                header: '状态'
              }, {
                accessorKey: 'api_token_set',
                header: '令牌'
              }, {
                accessorKey: 'cpu_usage',
                header: 'CPU'
              }, {
                accessorKey: 'mem_usage',
                header: '内存'
              }, {
                accessorKey: 'actions',
                header: '操作'
              }]"
            >
              <template #zone-cell="{ row }">
                <span class="text-sm">{{ row.original.zone.name }}</span>
              </template>

              <template #node-cell="{ row }">
                <div class="flex flex-col">
                  <span class="font-medium">{{ row.original.node.name }}</span>
                  <span class="text-xs text-muted">{{ row.original.node.api_user }}</span>
                </div>
              </template>

              <template #host-cell="{ row }">
                <code class="text-sm">{{ row.original.node.host }}:{{ row.original.node.port }}</code>
              </template>

              <template #enabled-cell="{ row }">
                <UBadge
                  :label="row.original.node.enabled ? '启用' : '禁用'"
                  :color="row.original.node.enabled ? 'success' : 'neutral'"
                  variant="subtle"
                />
              </template>

              <template #api_token_set-cell="{ row }">
                <UBadge
                  :label="row.original.node.api_token_set ? '已配置' : '未配置'"
                  :color="row.original.node.api_token_set ? 'success' : 'warning'"
                  variant="subtle"
                />
              </template>

              <!-- CPU 使用率：状态加载中显示占位；节点不可达（503）展示"不可达"徽标与错误信息 -->
              <template #cpu_usage-cell="{ row }">
                <div
                  v-if="statusEntry(row.original.node.id)?.data"
                  class="text-sm"
                >
                  {{ formatPercent(statusEntry(row.original.node.id)!.data!.status.cpu.usage) }}
                </div>
                <div
                  v-else-if="statusEntry(row.original.node.id)?.error"
                  class="flex items-center gap-1.5"
                >
                  <UBadge
                    label="不可达"
                    color="error"
                    variant="subtle"
                    :title="statusEntry(row.original.node.id)!.error!.message"
                  />
                </div>
                <span
                  v-else
                  class="text-sm text-muted"
                >-</span>
              </template>

              <!-- 内存使用率：同 CPU 列，不可达时展示"不可达"徽标 -->
              <template #mem_usage-cell="{ row }">
                <div
                  v-if="statusEntry(row.original.node.id)?.data"
                  class="text-sm"
                >
                  {{ formatPercent(statusEntry(row.original.node.id)!.data!.status.memory.usage) }}
                </div>
                <div
                  v-else-if="statusEntry(row.original.node.id)?.error"
                  class="flex items-center gap-1.5"
                >
                  <UBadge
                    label="不可达"
                    color="error"
                    variant="subtle"
                    :title="statusEntry(row.original.node.id)!.error!.message"
                  />
                </div>
                <span
                  v-else
                  class="text-sm text-muted"
                >-</span>
              </template>

              <template #actions-cell="{ row }">
                <UButton
                  icon="i-lucide-eye"
                  label="详情"
                  color="neutral"
                  variant="ghost"
                  size="sm"
                  :to="`/zones/${row.original.zone.id}/nodes/${row.original.node.id}`"
                />
                <UButton
                  icon="i-lucide-settings-2"
                  label="管理"
                  color="primary"
                  variant="ghost"
                  size="sm"
                  :to="`/zones/${row.original.zone.id}/nodes`"
                />
              </template>
            </UTable>
          </UCard>

          <AppEmpty
            v-else
            title="暂无节点"
            description="请先在「可用区」创建区域，再进入区域节点页登记 PVE 节点。"
            icon="i-lucide-server"
          />
        </template>
      </div>
    </template>
  </UDashboardPanel>
</template>
