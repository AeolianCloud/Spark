<script setup lang="ts">
// 节点总览：跨可用区浏览全部节点。
// 设计：listZones 响应内联各区域的完整节点列表（契约保证 nodes 恒为数组），
// 因此仅需一次列表调用即可展开全部节点，无需对每个 Zone 扇出节点查询。
import { listZones } from '~/api/zones'
import type { ApiError } from '~/api/client'
import type { NodeResponse, ZoneResponse } from '~/api/types'

const loading = ref(true)
const error = ref<ApiError | null>(null)
const zones = ref<ZoneResponse[]>([])

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    // 分页上限 100：可用区超过 100 个时按第一页口径统计
    const res = await listZones({ limit: 100 })
    zones.value = res.data
  } catch (err) {
    error.value = err as ApiError
  } finally {
    loading.value = false
  }
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
      </UDashboardNavbar>
    </template>

    <template #body>
      <div class="space-y-4 p-4">
        <AppErrorAlert
          v-if="error"
          :code="error.code"
          :message="error.message"
          title="节点列表加载失败"
        />

        <AppLoading
          v-else-if="loading"
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

              <template #actions-cell="{ row }">
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
