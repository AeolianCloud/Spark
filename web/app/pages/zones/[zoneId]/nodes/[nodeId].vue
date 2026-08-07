<script setup lang="ts">
/**
 * 节点详情页（5.2）：独立调用 GET /nodes/:id/status 获取配置 + 实时状态，不依赖列表页传参。
 * 节点不存在返回 404 not_found；PVE 不可达/令牌无效返回 503 node_unavailable（消息已脱敏），
 * 展示降级提示而非伪造状态；手动刷新保留旧数据（与 vms/[id].vue 模式一致）。
 */
import { ApiError, getNodeStatus } from '~/api'
import type { NodeStatusResponse } from '~/api'
import { useCatalog } from '~/composables/useCatalog'
import { formatDateTime, formatPercent } from '~/utils/format'
import { formatBytes, formatUptime } from '~/utils/vm'

const route = useRoute()
const { zoneName, refresh: refreshCatalog } = useCatalog()

// 路由参数：/zones/:zoneId/nodes/:nodeId；非法值（NaN/非正整数）防御：不向 API 发请求
const zoneId = computed(() => {
  const n = Number(route.params.zoneId)
  return Number.isInteger(n) && n > 0 ? n : 0
})
const nodeId = computed(() => {
  const n = Number(route.params.nodeId)
  return Number.isInteger(n) && n > 0 ? n : 0
})

// ---- 详情数据 ----
const node = ref<NodeStatusResponse | null>(null)
const loading = ref(true)
const error = ref<ApiError | null>(null)

// 请求序号守卫：快速切换路由（组件复用）时丢弃过期响应，防止旧节点数据覆盖新节点
let fetchSeq = 0

async function load(): Promise<void> {
  const id = nodeId.value
  const seq = ++fetchSeq
  loading.value = true
  error.value = null
  // 路由参数非法：直接展示错误，避免以 NaN/0 发起请求
  if (!id) {
    error.value = new ApiError(400, 'bad_request', '节点 ID 不合法')
    loading.value = false
    return
  }
  try {
    const res = await getNodeStatus(id)
    // 过期响应丢弃：期间已发起更新的请求，或路由已切换到其他节点
    if (seq !== fetchSeq || id !== nodeId.value) return
    node.value = res.data
  } catch (err) {
    if (seq !== fetchSeq || id !== nodeId.value) return
    // 刷新失败保留旧数据，仅展示错误条（首次加载无数据时自然展示错误态）
    error.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (seq === fetchSeq) loading.value = false
  }
}

// 路由参数变化（如从列表进入另一节点时组件复用）→ 清空旧数据并重新拉取
watch(() => route.params.nodeId, () => {
  node.value = null
  error.value = null
  void load()
})

onMounted(() => {
  void load()
  // 目录映射刷新（失败不阻塞详情，映射缺失时展示 id 并注明）：进入页面即重拉
  void refreshCatalog()
})

// ---- 展示派生 ----
// 在线状态徽标：online 绿色"在线"，offline/unknown 及未知状态给警告样式
const onlineBadge = computed(() => {
  const s = node.value?.status.status
  if (s === 'online') return { label: '在线', color: 'success' as const }
  if (s === 'offline' || s === 'unknown') return { label: s === 'offline' ? '离线' : '未知', color: 'warning' as const }
  return { label: s ?? '—', color: 'warning' as const }
})

// 可用内存 = 总量 - 已用（契约仅返回 total/used/usage）
const memAvailable = computed(() => {
  const memory = node.value?.status.memory
  if (!memory) return 0
  return memory.total - memory.used
})

// 所属可用区名称：目录映射缺失时回退展示 ID
const zoneLabel = computed(() => {
  const zid = node.value?.zone_id
  if (zid === undefined) return '—'
  return zoneName(zid) ?? `Zone #${zid}（名称未加载）`
})

// UProgress 值域 0-100：usage 契约约束为 0-1，防御超界值（与 formatPercent 的 clamp 逻辑一致）
function clampPercent(n: number): number {
  if (!Number.isFinite(n)) return 0
  return Math.min(100, Math.max(0, Math.round(n)))
}

const cpuPercent = computed(() => clampPercent((node.value?.status.cpu.usage ?? 0) * 100))
const memPercent = computed(() => clampPercent((node.value?.status.memory.usage ?? 0) * 100))
const diskPercent = computed(() => clampPercent((node.value?.status.disk.usage ?? 0) * 100))
</script>

<template>
  <UDashboardPanel>
    <template #header>
      <UDashboardNavbar :title="node ? node.name : '节点详情'">
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
        <!-- 返回列表 -->
        <UButton
          icon="i-lucide-arrow-left"
          variant="ghost"
          size="sm"
          :to="`/zones/${zoneId}/nodes`"
        >
          返回节点列表
        </UButton>

        <!-- 加载态骨架屏：仅首次加载（无数据）时显示；手动刷新保留旧数据，避免闪烁 -->
        <AppLoading
          v-if="!node && loading"
          :rows="6"
        />

        <template v-else>
          <!-- 加载失败：503 node_unavailable 等，展示降级提示不伪造状态 -->
          <AppErrorAlert
            v-if="error"
            :code="error.code"
            :message="error.message"
            :title="error.code === 'node_unavailable' ? '节点不可达' : '详情加载失败'"
          />

          <template v-else-if="node">
            <!-- 配置信息 -->
            <UCard>
              <template #header>
                配置信息
              </template>
              <div class="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-3">
                <div>
                  <div class="text-xs text-muted">
                    节点名称
                  </div>
                  <div class="text-sm">
                    {{ node.name }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    PVE 节点名
                  </div>
                  <div class="text-sm">
                    {{ node.pve_name || node.name }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    主机地址
                  </div>
                  <code class="text-sm">{{ node.host }}:{{ node.port }}</code>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    API 用户
                  </div>
                  <div class="text-sm">
                    {{ node.api_user }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    API 令牌
                  </div>
                  <UBadge
                    :label="node.api_token_set ? '已配置' : '未配置'"
                    :color="node.api_token_set ? 'success' : 'warning'"
                    variant="subtle"
                  />
                </div>
                <div>
                  <div class="text-xs text-muted">
                    启用状态
                  </div>
                  <UBadge
                    :label="node.enabled ? '启用' : '禁用'"
                    :color="node.enabled ? 'success' : 'neutral'"
                    variant="subtle"
                  />
                </div>
                <div>
                  <div class="text-xs text-muted">
                    所属可用区
                  </div>
                  <div class="text-sm">
                    {{ zoneLabel }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    创建时间
                  </div>
                  <div class="text-sm">
                    {{ formatDateTime(node.created_at) }}
                  </div>
                </div>
              </div>
            </UCard>

            <!-- 实时状态（PVE 穿透：CPU/内存/磁盘/集群信息） -->
            <UCard>
              <template #header>
                <div class="flex items-center justify-between">
                  <span>实时状态</span>
                  <UBadge
                    :label="onlineBadge.label"
                    :color="onlineBadge.color"
                    variant="subtle"
                  />
                </div>
              </template>
              <div class="space-y-4">
                <!-- CPU -->
                <div>
                  <div class="mb-1 flex items-center justify-between text-sm">
                    <span>CPU（{{ node.status.cpu.cores }} 核）</span>
                    <span>{{ formatPercent(node.status.cpu.usage) }}</span>
                  </div>
                  <UProgress
                    :model-value="cpuPercent"
                    size="sm"
                    color="success"
                  />
                  <div class="mt-1 text-xs text-muted">
                    负载：{{ node.status.cpu.loadavg.join(' / ') }}
                  </div>
                </div>

                <!-- 内存 -->
                <div>
                  <div class="mb-1 flex items-center justify-between text-sm">
                    <span>内存</span>
                    <span>
                      {{ formatBytes(node.status.memory.used) }} / {{ formatBytes(node.status.memory.total) }}
                      （{{ formatPercent(node.status.memory.usage) }}）
                    </span>
                  </div>
                  <UProgress
                    :model-value="memPercent"
                    size="sm"
                    color="info"
                  />
                  <div class="mt-1 text-xs text-muted">
                    可用：{{ formatBytes(memAvailable) }}
                  </div>
                </div>

                <!-- 磁盘（根分区） -->
                <div>
                  <div class="mb-1 flex items-center justify-between text-sm">
                    <span>根分区磁盘</span>
                    <span>
                      {{ formatBytes(node.status.disk.used) }} / {{ formatBytes(node.status.disk.total) }}
                      （{{ formatPercent(node.status.disk.usage) }}）
                    </span>
                  </div>
                  <UProgress
                    :model-value="diskPercent"
                    size="sm"
                    color="warning"
                  />
                </div>

                <!-- 集群信息 -->
                <div class="grid grid-cols-1 gap-x-6 gap-y-3 border-t border-(--ui-border) pt-4 sm:grid-cols-2 lg:grid-cols-3">
                  <div>
                    <div class="text-xs text-muted">
                      PVE 版本
                    </div>
                    <div class="text-sm">
                      {{ node.status.pve_version }}
                    </div>
                  </div>
                  <div>
                    <div class="text-xs text-muted">
                      内核版本
                    </div>
                    <div class="text-sm">
                      {{ node.status.kernel_version }}
                    </div>
                  </div>
                  <div>
                    <div class="text-xs text-muted">
                      在线时长
                    </div>
                    <div class="text-sm">
                      {{ formatUptime(node.status.uptime_seconds) }}
                    </div>
                  </div>
                </div>

                <!-- 网络吞吐（节点级，PVE rrddata 最近采样点，bytes/s） -->
                <div class="border-t border-(--ui-border) pt-4">
                  <div class="mb-2 text-xs text-muted">
                    网络吞吐
                  </div>
                  <div class="flex gap-6 text-sm">
                    <span class="flex items-center gap-1">
                      <UIcon
                        name="i-lucide-arrow-down"
                        class="text-(--ui-success)"
                      />
                      入向 {{ formatBytes(node.status.net_io.net_in) }}/s
                    </span>
                    <span class="flex items-center gap-1">
                      <UIcon
                        name="i-lucide-arrow-up"
                        class="text-(--ui-primary)"
                      />
                      出向 {{ formatBytes(node.status.net_io.net_out) }}/s
                    </span>
                  </div>
                </div>
              </div>
            </UCard>

            <!-- 网络接口 -->
            <UCard
              :ui="{ body: 'p-0' }"
            >
              <template #header>
                网络接口
              </template>
              <UTable
                :data="node.status.network"
                :columns="[{
                  accessorKey: 'iface',
                  header: '接口'
                }, {
                  accessorKey: 'type',
                  header: '类型'
                }, {
                  accessorKey: 'address',
                  header: 'IP 地址'
                }, {
                  accessorKey: 'active',
                  header: '活跃'
                }]"
              >
                <template #active-cell="{ row }">
                  <UBadge
                    v-if="row.original.active !== null"
                    :label="row.original.active ? '活跃' : '非活跃'"
                    :color="row.original.active ? 'success' : 'neutral'"
                    variant="subtle"
                  />
                  <span
                    v-else
                    class="text-sm text-muted"
                  >未知</span>
                </template>
              </UTable>
            </UCard>
          </template>
        </template>
      </div>
    </template>
  </UDashboardPanel>
</template>
