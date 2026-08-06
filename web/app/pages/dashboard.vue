<script setup lang="ts">
// Dashboard 概览：Zone/Node/VM 统计与 IP 池占用概览，数据全部来自列表 API。
// 说明：listVMs 为穿透式查询（每台 VM 向 PVE 扇出一次），为避免放大扇出，
// 本页仅请求一页（limit 上限 100）：VM 总数为 X-Total-Count 头（全量），
// 状态分布仅统计第一页数据，超过一页时以注释口径近似，不拉取全量数据。
import { listZones } from '~/api/zones'
import { listVMs } from '~/api/vms'
import { listPools } from '~/api/pools'
import type { ApiError } from '~/api/client'
import type { Pool } from '~/api/types'
import { vmStatusBadge } from '~/utils/vm'

const loading = ref(true)
// 三路接口各自独立报错，单路失败不影响其余卡片展示
const zonesError = ref<ApiError | null>(null)
const vmsError = ref<ApiError | null>(null)
const poolsError = ref<ApiError | null>(null)

// Zone/Node 统计：nodeTotal 由 listZones 返回的各区域完整节点列表累加（第一页口径）
const zoneTotal = ref(0)
const nodeTotal = ref(0)
// VM 统计：vmTotal 为 X-Total-Count 全量总数；状态分布按第一页统计
const vmTotal = ref(0)
const vmStatusCounts = ref<Record<string, number>>({})
// IP 池概览：池列表（含网段/网关/DNS）
const pools = ref<Pool[]>([])

/** 已知 VM 状态中文标签（未知状态归入"其他"，见 vmStatusLabel） */
const KNOWN_STATUSES = ['running', 'stopped', 'paused', 'suspended', 'creating', 'failed'] as const

async function load(): Promise<void> {
  loading.value = true
  zonesError.value = null
  vmsError.value = null
  poolsError.value = null
  // Promise.allSettled：并行发起且各自独立捕获错误，避免单接口失败影响整页
  const [zonesRes, vmsRes, poolsRes] = await Promise.allSettled([
    listZones({ limit: 100 }),
    listVMs({ limit: 100 }),
    listPools({ limit: 100 })
  ])

  if (zonesRes.status === 'fulfilled') {
    zoneTotal.value = zonesRes.value.total
    // listZones 分页上限 100，Zone 超过 100 个时节点总数按第一页统计
    nodeTotal.value = zonesRes.value.data.reduce((sum, zone) => sum + zone.nodes.length, 0)
  } else {
    zonesError.value = zonesRes.reason as ApiError
  }

  if (vmsRes.status === 'fulfilled') {
    vmTotal.value = vmsRes.value.total
    const counts: Record<string, number> = {}
    for (const vm of vmsRes.value.data.vms) {
      counts[vm.status] = (counts[vm.status] ?? 0) + 1
    }
    vmStatusCounts.value = counts
  } else {
    vmsError.value = vmsRes.reason as ApiError
  }

  if (poolsRes.status === 'fulfilled') {
    pools.value = poolsRes.value.data
  } else {
    poolsError.value = poolsRes.reason as ApiError
  }

  loading.value = false
}

onMounted(load)

/** 状态分布展示行：按已知状态固定顺序排列，未知状态归入"其他" */
const statusRows = computed(() => {
  const rows: Array<{ status: string, label: string, count: number }> = []
  for (const status of KNOWN_STATUSES) {
    const count = vmStatusCounts.value[status] ?? 0
    if (count > 0) rows.push({ status, label: vmStatusLabel(status), count })
  }
  const otherCount = Object.entries(vmStatusCounts.value)
    .filter(([status]) => !(KNOWN_STATUSES as readonly string[]).includes(status))
    .reduce((sum, [, count]) => sum + count, 0)
  if (otherCount > 0) rows.push({ status: 'other', label: '其他', count: otherCount })
  return rows
})
</script>

<template>
  <UDashboardPanel>
    <template #header>
      <UDashboardNavbar title="Dashboard">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #trailing>
          <UButton
            icon="i-lucide-refresh-cw"
            variant="outline"
            :loading="loading"
            :disabled="loading"
            @click="load"
          >
            刷新
          </UButton>
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <div class="space-y-4 p-4">
        <AppLoading
          v-if="loading"
          :rows="6"
        />

        <template v-else>
          <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <!-- Zone 与 Node 统计卡片 -->
            <UCard>
              <template #header>
                <div class="flex items-center gap-2">
                  <UIcon
                    name="i-lucide-boxes"
                    class="h-4 w-4 text-muted"
                  />
                  <span class="text-sm font-medium">可用区 / 节点</span>
                </div>
              </template>
              <AppErrorAlert
                v-if="zonesError"
                :code="zonesError.code"
                :message="zonesError.message"
                title="可用区统计加载失败"
              />
              <template v-else>
                <div class="flex items-end justify-between">
                  <div>
                    <p class="text-2xl font-bold">
                      {{ zoneTotal }}
                    </p>
                    <p class="text-xs text-muted">
                      Zone 总数
                    </p>
                  </div>
                  <div class="text-right">
                    <p class="text-2xl font-bold">
                      {{ nodeTotal }}
                    </p>
                    <p class="text-xs text-muted">
                      Node 总数（当前页口径）
                    </p>
                  </div>
                </div>
              </template>
            </UCard>

            <!-- VM 统计卡片：总数 + 状态分布 -->
            <UCard>
              <template #header>
                <div class="flex items-center gap-2">
                  <UIcon
                    name="i-lucide-monitor"
                    class="h-4 w-4 text-muted"
                  />
                  <span class="text-sm font-medium">虚拟机</span>
                </div>
              </template>
              <AppErrorAlert
                v-if="vmsError"
                :code="vmsError.code"
                :message="vmsError.message"
                title="虚拟机统计加载失败"
              />
              <template v-else>
                <p class="text-2xl font-bold">
                  {{ vmTotal }}
                </p>
                <p class="text-xs text-muted mb-3">
                  VM 总数（穿透式统计，状态分布取第一页）
                </p>
                <div class="flex flex-wrap gap-1.5">
                  <UBadge
                    v-for="row in statusRows"
                    :key="row.status"
                    :label="`${row.label} ${row.count}`"
                    :color="vmStatusBadge(row.status).color"
                    variant="subtle"
                  />
                  <UBadge
                    v-if="statusRows.length === 0"
                    label="暂无虚拟机"
                    color="neutral"
                    variant="subtle"
                  />
                </div>
              </template>
            </UCard>

            <!-- IP 池占用概览卡片：池数量 + 各池网段 -->
            <UCard>
              <template #header>
                <div class="flex items-center gap-2">
                  <UIcon
                    name="i-lucide-network"
                    class="h-4 w-4 text-muted"
                  />
                  <span class="text-sm font-medium">IP 池占用概览</span>
                </div>
              </template>
              <AppErrorAlert
                v-if="poolsError"
                :code="poolsError.code"
                :message="poolsError.message"
                title="IP 池概览加载失败"
              />
              <template v-else>
                <p class="text-2xl font-bold">
                  {{ pools.length }}
                </p>
                <p class="text-xs text-muted mb-3">
                  IP 池数量
                </p>
                <div
                  v-if="pools.length > 0"
                  class="space-y-1.5 max-h-48 overflow-y-auto"
                >
                  <div
                    v-for="pool in pools"
                    :key="pool.id"
                    class="flex items-center justify-between gap-2 rounded-md bg-muted/50 px-2 py-1 text-sm"
                  >
                    <span class="truncate font-medium">{{ pool.name }}</span>
                    <code class="shrink-0 text-xs">{{ pool.network_cidr }}</code>
                  </div>
                </div>
                <p
                  v-else
                  class="text-sm text-muted"
                >
                  暂无 IP 池
                </p>
              </template>
            </UCard>
          </div>
        </template>
      </div>
    </template>
  </UDashboardPanel>
</template>
