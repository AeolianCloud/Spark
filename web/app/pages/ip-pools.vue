<script setup lang="ts">
// IP 池管理：列表 + 创建 + 池内节点白名单配置。
// 契约缺口：docs/openapi.yaml 的 /ip-pools 仅定义 POST（创建）与 GET（列表），
// /ip-pools/{id}/nodes 仅定义 PUT（整体替换白名单）与 GET；无 updatePool/deletePool 端点，
// 故本页不实现 IP 池的更新与删除功能。
import { createPool, getPoolNodes, listPools, setPoolNodes } from '~/api/pools'
import { listZones } from '~/api/zones'
import { listNodesByZone } from '~/api/nodes'
import type { ApiError } from '~/api/client'
import type { NodeResponse, Pool, ZoneResponse } from '~/api/types'
import type { FormValidateError } from '~/utils/format'

const toast = useToast()

// 列表状态：池列表 + 区域列表（区域仅用于 zone_id → 区域名映射）
const loading = ref(true)
const error = ref<ApiError | null>(null)
const pools = ref<Pool[]>([])
const zones = ref<ZoneResponse[]>([])
const total = ref(0)
// 一次拉取上限 100（契约 limit 上限），超过时按第一页口径展示，总数以 X-Total-Count 为准
const PAGE_LIMIT = 100

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  // 池列表与区域列表并行获取；区域查询失败仅影响"区域名"列展示（回退显示 zone_id）
  const [poolsRes, zonesRes] = await Promise.allSettled([
    listPools({ limit: PAGE_LIMIT }),
    listZones({ limit: PAGE_LIMIT })
  ])

  if (poolsRes.status === 'fulfilled') {
    pools.value = poolsRes.value.data
    total.value = poolsRes.value.total
  } else {
    error.value = poolsRes.reason as ApiError
  }

  if (zonesRes.status === 'fulfilled') {
    zones.value = zonesRes.value.data
  }

  loading.value = false
}

onMounted(load)

/** zone_id → 区域名映射（找不到时回退显示 ID） */
function zoneNameOf(zoneId: number): string {
  return zones.value.find(z => z.id === zoneId)?.name ?? `#${zoneId}`
}

// 创建表单
const createOpen = ref(false)
const creating = ref(false)
const createError = ref<ApiError | null>(null)
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const createFormRef = ref<{ submit: () => Promise<void> }>()
const createForm = reactive({
  zone_id: 0,
  name: '',
  network_cidr: '',
  gateway: '',
  dns: ''
})

/** 区域下拉选项（USelect items：label + value） */
const zoneOptions = computed(() =>
  zones.value.map(z => ({ label: z.name, value: z.id }))
)

// 轻量格式校验（前端拦截，后端另有契约校验兜底）
// IPv4：四段 0-255；段以 (0|[1-9]\d{0,2}) 拒绝前导零（如 01.1.1.1），与后端 netip.ParseAddr 严格口径一致
const IPV4_RE = /^(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})$/
// CIDR：IPv4 + /前缀长度（0-32），同样拒绝前导零
const CIDR_RE = /^(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\/(\d{1,2})$/

/** 校验 IPv4 地址：格式 + 每段 ≤ 255 */
function isValidIpv4(s: string): boolean {
  const m = s.match(IPV4_RE)
  if (!m) return false
  return m.slice(1).every(part => Number(part) <= 255)
}

/** 校验 CIDR：IPv4 + 前缀长度 ≤ 32 */
function isValidCidr(s: string): boolean {
  const m = s.match(CIDR_RE)
  if (!m) return false
  if (m.slice(1, 5).some(part => Number(part) > 255)) return false
  return Number(m[5]) <= 32
}

function validateCreate(): FormValidateError[] {
  const errors: FormValidateError[] = []
  if (!createForm.zone_id) errors.push({ name: 'zone_id', message: '请选择所属可用区' })
  if (!createForm.name.trim()) errors.push({ name: 'name', message: '请输入池名称' })
  if (!createForm.network_cidr.trim()) {
    errors.push({ name: 'network_cidr', message: '请输入网段（CIDR）' })
  } else if (!isValidCidr(createForm.network_cidr.trim())) {
    errors.push({ name: 'network_cidr', message: '网段格式不正确，应为如 10.9.0.0/24' })
  }
  if (!createForm.gateway.trim()) {
    errors.push({ name: 'gateway', message: '请输入网关地址' })
  } else if (!isValidIpv4(createForm.gateway.trim())) {
    errors.push({ name: 'gateway', message: '网关格式不正确，应为 IPv4 地址' })
  }
  if (!createForm.dns.trim()) {
    errors.push({ name: 'dns', message: '请输入 DNS 服务器' })
  } else if (!isValidIpv4(createForm.dns.trim())) {
    errors.push({ name: 'dns', message: 'DNS 格式不正确，应为 IPv4 地址' })
  }
  return errors
}

function resetCreateForm(): void {
  createForm.zone_id = 0
  createForm.name = ''
  createForm.network_cidr = ''
  createForm.gateway = ''
  createForm.dns = ''
  createError.value = null
}

function openCreateModal(): void {
  resetCreateForm()
  createOpen.value = true
}

async function onSubmitCreate(): Promise<void> {
  const createdName = createForm.name.trim()
  creating.value = true
  createError.value = null
  try {
    await createPool({
      zone_id: createForm.zone_id,
      name: createdName,
      network_cidr: createForm.network_cidr.trim(),
      gateway: createForm.gateway.trim(),
      dns: createForm.dns.trim()
    })
    createOpen.value = false
    resetCreateForm()
    toast.add({ title: '创建成功', description: `IP 池「${createdName}」已创建`, color: 'success' })
    await load()
  } catch (err) {
    createError.value = err as ApiError
  } finally {
    creating.value = false
  }
}

// 节点白名单配置：候选节点（所属 Zone 全部节点）+ 已选节点（getPoolNodes）整体替换（setPoolNodes）
const whitelistOpen = ref(false)
const whitelistPool = ref<Pool | null>(null)
const whitelistLoading = ref(false)
const whitelistSaving = ref(false)
// 加载失败与保存失败分开承载：标题与展示位置不同，避免误导
const whitelistLoadError = ref<ApiError | null>(null)
const whitelistSaveError = ref<ApiError | null>(null)
const candidateNodes = ref<NodeResponse[]>([])
const selectedNodeIds = ref<number[]>([])
// 候选节点 → CheckboxGroup items：单独 UCheckbox 是 reka-ui 单值模式（v-model 数组会被覆盖为布尔值），
// 必须经 UCheckboxGroup（items prop）承载多选；valueKey 指向 id（number）以保持 node_ids 数组语义
// （CheckboxGroupItem 的 value 键仅接受 string，故用 value-key 声明自定义值键）
const whitelistNodeOptions = computed<{ id: number, label: string, description?: string }[]>(() =>
  candidateNodes.value.map(n => ({
    id: n.id,
    label: `${n.name}（${n.host}:${n.port}）`,
    description: n.enabled ? '已启用' : '已禁用'
  }))
)
// 请求序号守卫：快速"打开A→关闭→打开B"时，丢弃 A 的过期响应，防止 A 的节点覆盖 B 的白名单
let whitelistSeq = 0

async function openWhitelistModal(pool: Pool): Promise<void> {
  const seq = ++whitelistSeq
  whitelistPool.value = pool
  whitelistLoadError.value = null
  whitelistSaveError.value = null
  whitelistOpen.value = true
  whitelistLoading.value = true
  // 候选节点（该 Zone 全部节点）与当前白名单并行获取
  const [candidatesRes, currentRes] = await Promise.allSettled([
    listNodesByZone(pool.zone_id),
    getPoolNodes(pool.id)
  ])
  // 过期响应一律丢弃：期间已切换（或关闭后重开）到其他池
  if (seq !== whitelistSeq) return

  if (candidatesRes.status === 'fulfilled') {
    candidateNodes.value = candidatesRes.value.data
  } else {
    whitelistLoadError.value = candidatesRes.reason as ApiError
  }

  if (currentRes.status === 'fulfilled') {
    selectedNodeIds.value = currentRes.value.data.map(n => n.id)
  } else if (!whitelistLoadError.value) {
    whitelistLoadError.value = currentRes.reason as ApiError
  }

  whitelistLoading.value = false
}

async function onSubmitWhitelist(): Promise<void> {
  // 快照当前池：保存期间弹窗可能已关闭并切换/重开其他池，过期响应按池守卫丢弃
  const pool = whitelistPool.value
  if (!pool || whitelistSaving.value) return
  whitelistSaving.value = true
  whitelistSaveError.value = null
  try {
    await setPoolNodes(pool.id, { node_ids: selectedNodeIds.value })
    // 仅当仍是同一池时才关弹窗并 toast：期间切到 B 时 A 的响应不得关闭 B 的弹窗
    if (whitelistPool.value === pool) {
      whitelistOpen.value = false
      toast.add({
        title: '白名单已保存',
        description: `「${pool.name}」白名单已替换（${selectedNodeIds.value.length} 个节点）`,
        color: 'success'
      })
    }
  } catch (err) {
    // 仅当仍是同一池时展示错误：A 保存失败不得污染 B 的错误区
    if (whitelistPool.value === pool) {
      whitelistSaveError.value = err as ApiError
    }
  } finally {
    // 始终复位（无论是否已切换池），避免新池弹窗的保存按钮被在途状态卡死
    whitelistSaving.value = false
  }
}
</script>

<template>
  <UDashboardPanel>
    <template #header>
      <UDashboardNavbar title="IP 池">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #trailing>
          <UButton
            icon="i-lucide-plus"
            color="primary"
            @click="openCreateModal"
          >
            创建 IP 池
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
          title="IP 池列表加载失败"
        />

        <AppLoading
          v-else-if="loading"
          :rows="4"
        />

        <template v-else>
          <p class="px-1 text-sm text-muted">
            共 {{ total }} 个 IP 池（契约无更新/删除端点，仅支持创建与白名单配置）
          </p>

          <UCard
            v-if="pools.length > 0"
            :ui="{ body: 'p-0' }"
          >
            <UTable
              :data="pools"
              :columns="[{
                accessorKey: 'name',
                header: '池名'
              }, {
                accessorKey: 'zone_id',
                header: '所属可用区'
              }, {
                accessorKey: 'network_cidr',
                header: '网段'
              }, {
                accessorKey: 'gateway',
                header: '网关'
              }, {
                accessorKey: 'dns',
                header: 'DNS'
              }, {
                accessorKey: 'created_at',
                header: '创建时间'
              }, {
                accessorKey: 'actions',
                header: '操作'
              }]"
            >
              <template #name-cell="{ row }">
                <span class="font-medium">{{ row.original.name }}</span>
              </template>

              <template #zone_id-cell="{ row }">
                <span class="text-sm">{{ zoneNameOf(row.original.zone_id) }}</span>
              </template>

              <template #network_cidr-cell="{ row }">
                <code class="text-sm">{{ row.original.network_cidr }}</code>
              </template>

              <template #gateway-cell="{ row }">
                <code class="text-sm">{{ row.original.gateway }}</code>
              </template>

              <template #dns-cell="{ row }">
                <code class="text-sm">{{ row.original.dns }}</code>
              </template>

              <template #created_at-cell="{ row }">
                <span class="text-sm text-muted">{{ formatDateTime(row.original.created_at) }}</span>
              </template>

              <template #actions-cell="{ row }">
                <UButton
                  icon="i-lucide-shield-check"
                  label="节点白名单"
                  color="primary"
                  variant="ghost"
                  size="sm"
                  @click="openWhitelistModal(row.original)"
                />
              </template>
            </UTable>
          </UCard>

          <AppEmpty
            v-else
            title="暂无 IP 池"
            description="点击右上角「创建 IP 池」登记网段，创建后自动展开为逐地址记录。"
            icon="i-lucide-network"
          />
        </template>
      </div>

      <!-- BUG-1：UModal 默认插槽是 DialogTrigger（as-child 内联渲染），内容放默认插槽会内联铺在页面上、
           点击提交还会连带触发打开空弹窗；因此弹窗内容必须进 #body、按钮进 #footer。
           UModal 位于 panel 的 #body 具名插槽末尾（panel 默认插槽为空，不触发插槽冲突）；
           且 UModal 默认 portal 渲染到 body，DOM 位置不影响表现，弹窗只在点击按钮后打开。
           lint 约束：vue/no-multiple-template-root 禁止多根，UModal 无法作为 panel 的兄弟根。 -->
      <UModal
        v-model:open="createOpen"
        title="创建 IP 池"
        :description="'网段将自动展开为逐地址记录（如 10.9.0.0/24 → 254 个可用地址）'"
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
              name="zone_id"
              label="所属可用区"
              required
            >
              <USelect
                v-model="createForm.zone_id"
                :items="zoneOptions"
                placeholder="选择可用区"
              />
            </UFormField>

            <UFormField
              name="name"
              label="池名称"
              required
            >
              <UInput
                v-model="createForm.name"
                placeholder="如 prod-net"
              />
            </UFormField>

            <UFormField
              name="network_cidr"
              label="网段（CIDR）"
              required
              :hint="'如 10.9.0.0/24'"
            >
              <UInput
                v-model="createForm.network_cidr"
                placeholder="如 10.9.0.0/24"
              />
            </UFormField>

            <UFormField
              name="gateway"
              label="网关"
              required
            >
              <UInput
                v-model="createForm.gateway"
                placeholder="如 10.9.0.1"
              />
            </UFormField>

            <UFormField
              name="dns"
              label="DNS 服务器"
              required
            >
              <UInput
                v-model="createForm.dns"
                placeholder="如 1.1.1.1"
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
            @click="createOpen = false"
          />
          <UButton
            label="创建"
            color="primary"
            icon="i-lucide-plus"
            :loading="creating"
            @click="createFormRef?.submit()"
          />
        </template>
      </UModal>

      <!-- 节点白名单配置弹窗（BUG-1：UModal 移出 panel 作为兄弟节点） -->
      <UModal
        v-model:open="whitelistOpen"
        :title="`节点白名单 · ${whitelistPool?.name ?? ''}`"
        :description="'勾选允许从该池分配 IP 的节点；保存时整体替换白名单（setPoolNodes）'"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <div class="space-y-4">
            <AppErrorAlert
              v-if="whitelistLoadError"
              :code="whitelistLoadError.code"
              :message="whitelistLoadError.message"
              title="白名单加载失败"
            />

            <AppLoading
              v-else-if="whitelistLoading"
              :rows="4"
            />

            <template v-else>
              <p class="text-sm text-muted">
                候选节点（可用区「{{ zoneNameOf(whitelistPool?.zone_id ?? 0) }}」共 {{ candidateNodes.length }} 个），已选 {{ selectedNodeIds.length }} 个
              </p>

              <UScrollArea class="max-h-72">
                <div class="space-y-2 pr-3">
                  <!-- BUG-4 修复：UCheckbox 单独使用走 reka-ui 单值模式，勾选会把 selectedNodeIds
                       覆盖为布尔值（保存时 node_ids 非数组 → 后端 400）；
                       UCheckboxGroup（v4）无默认插槽，故以 items prop 承载多选 -->
                  <UCheckboxGroup
                    v-model="selectedNodeIds"
                    value-key="id"
                    :items="whitelistNodeOptions"
                  />
                  <p
                    v-if="candidateNodes.length === 0"
                    class="text-sm text-muted"
                  >
                    该可用区暂无节点，请先登记节点
                  </p>
                </div>
              </UScrollArea>

              <AppErrorAlert
                v-if="whitelistSaveError"
                :code="whitelistSaveError.code"
                :message="whitelistSaveError.message"
                title="白名单保存失败"
              />
            </template>
          </div>
        </template>

        <template #footer>
          <!-- 加载中不展示操作按钮，避免对未就绪状态操作 -->
          <template v-if="!whitelistLoading">
            <UButton
              label="取消"
              variant="outline"
              @click="whitelistOpen = false"
            />
            <UButton
              label="保存白名单"
              color="primary"
              icon="i-lucide-check"
              :loading="whitelistSaving"
              :disabled="candidateNodes.length === 0"
              @click="onSubmitWhitelist"
            />
          </template>
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
