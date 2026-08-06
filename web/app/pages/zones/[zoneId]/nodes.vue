<script setup lang="ts">
// 区域节点管理：节点列表 + 创建 + 编辑。
// 令牌只写不读（design D7 / ADR 0004）：api_token 表单为密码输入，提交后立即清空，
// 列表/编辑表单绝不回显令牌内容，仅展示 api_token_set 状态徽章；
// 编辑时 api_token 留空表示保留原密钥（契约语义）。
import { createNode, listNodesByZone, updateNode } from '~/api/nodes'
import { listZones } from '~/api/zones'
import { ApiError } from '~/api/client'
import type { NodeResponse } from '~/api/types'
import type { FormValidateError } from '~/utils/format'

const route = useRoute()
const toast = useToast()

// 路由参数：/zones/:zoneId/nodes；非法值（NaN/非正整数）防御：不向 API 发请求
const zoneId = computed(() => {
  const n = Number(route.params.zoneId)
  return Number.isInteger(n) && n > 0 ? n : 0
})
// 区域名（从列表接口查得，用于页面标题与面包屑）
const zoneName = ref('')

const loading = ref(true)
const error = ref<ApiError | null>(null)
const nodes = ref<NodeResponse[]>([])

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  // 路由参数非法：直接展示错误，避免以 NaN 发起请求
  if (!zoneId.value) {
    error.value = new ApiError(400, 'bad_request', '区域 ID 不合法')
    loading.value = false
    return
  }
  try {
    const nodesRes = await listNodesByZone(zoneId.value)
    nodes.value = nodesRes.data
  } catch (err) {
    error.value = err as ApiError
    loading.value = false
    return
  }
  // 区域名仅用于标题展示：查询失败不阻塞节点列表，回退显示区域 ID
  try {
    const zonesRes = await listZones({ limit: 100 })
    zoneName.value = zonesRes.data.find(z => z.id === zoneId.value)?.name ?? `#${zoneId.value}`
  } catch {
    zoneName.value = `#${zoneId.value}`
  }
  loading.value = false
}

onMounted(load)

// 节点表单（创建/编辑共用）：api_token 为只写字段，编辑时留空 = 保留原密钥
interface NodeFormState {
  name: string
  pve_name: string
  host: string
  api_user: string
  api_token: string
  enabled: boolean
}

const formOpen = ref(false)
const formMode = ref<'create' | 'edit'>('create')
const saving = ref(false)
const formError = ref<ApiError | null>(null)
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const formRef = ref<{ submit: () => Promise<void> }>()
// 编辑中的目标节点（仅用于定位 updateNode 的 id；令牌内容永不进入该对象）
const editingNode = ref<NodeResponse | null>(null)
const form = reactive<NodeFormState>({
  name: '',
  pve_name: '',
  host: '',
  api_user: '',
  api_token: '',
  enabled: true
})

function validateForm(): FormValidateError[] {
  const errors: FormValidateError[] = []
  if (!form.name.trim()) errors.push({ name: 'name', message: '请输入节点名称' })
  if (!form.host.trim()) errors.push({ name: 'host', message: '请输入节点主机地址' })
  if (!form.api_user.trim()) errors.push({ name: 'api_user', message: '请输入 PVE API 用户' })
  if (formMode.value === 'create' && !form.api_token) {
    errors.push({ name: 'api_token', message: '请输入 PVE API 令牌' })
  }
  return errors
}

function resetForm(): void {
  form.name = ''
  form.pve_name = ''
  form.host = ''
  form.api_user = ''
  // 令牌只写：任何一次提交/关闭后立即清空输入框，前端不持久化
  form.api_token = ''
  form.enabled = true
  formError.value = null
  editingNode.value = null
}

// 令牌只写安全红线：任何关闭路径（取消/ESC/遮罩点击/提交成功）都清空令牌输入框；
// 提交失败弹窗保持打开时保留令牌属合理重试 UX，但关闭必清（与 vms.vue watch(createOpen) 模式一致）
watch(formOpen, (open) => {
  if (!open) form.api_token = ''
})

function openCreateModal(): void {
  formMode.value = 'create'
  resetForm()
  formOpen.value = true
}

function openEditModal(node: NodeResponse): void {
  formMode.value = 'edit'
  resetForm()
  // 预填除令牌外的全部字段（令牌只写：编辑表单不预填，留空表示保留原密钥）
  form.name = node.name
  form.pve_name = node.pve_name
  // 编辑预填 host：契约 host 支持 :port 后缀（缺省 8006）；非默认端口必须拼入 host 回显，
  // 否则仅提交纯 host 会把非默认端口静默重置回 8006
  form.host = node.port !== 8006 ? `${node.host}:${node.port}` : node.host
  form.api_user = node.api_user
  form.enabled = node.enabled
  editingNode.value = node
  formOpen.value = true
}

async function onSubmitForm(): Promise<void> {
  const payload = {
    name: form.name.trim(),
    // pve_name 留空表示沿用业务名（契约缺省语义）
    pve_name: form.pve_name.trim() || undefined,
    host: form.host.trim(),
    api_user: form.api_user.trim(),
    // 编辑时留空 = 保留原密钥；创建时必填（validateForm 已校验）
    api_token: form.api_token,
    enabled: form.enabled
  }

  saving.value = true
  formError.value = null
  try {
    if (formMode.value === 'create') {
      // 路由参数非法防御：zoneId 无效（如 /zones/abc/nodes 解析为 0）时页面已展示错误，
      // 不得再以 0 发起 createNode（负载内 zone_id 缺失会被后端拒绝）
      if (!zoneId.value) {
        formError.value = new ApiError(400, 'bad_request', '区域 ID 不合法')
        return
      }
      await createNode(zoneId.value, payload)
      toast.add({ title: '创建成功', description: `节点「${form.name}」已登记`, color: 'success' })
    } else {
      await updateNode(editingNode.value!.id, payload)
      toast.add({ title: '保存成功', description: `节点「${form.name}」已更新`, color: 'success' })
    }
    formOpen.value = false
    // 令牌只写：提交成功后立即清空表单（含令牌输入框）
    resetForm()
    await load()
  } catch (err) {
    formError.value = err as ApiError
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <UDashboardPanel>
    <template #header>
      <UDashboardNavbar :title="`节点 · ${zoneName}`">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #trailing>
          <UButton
            icon="i-lucide-plus"
            color="primary"
            @click="openCreateModal"
          >
            登记节点
          </UButton>
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <div class="space-y-4 p-4">
        <UBreadcrumb
          :items="[{
            label: '可用区',
            icon: 'i-lucide-boxes',
            to: '/zones'
          }, {
            label: zoneName,
            to: '/zones'
          }, {
            label: '节点'
          }]"
        />

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
          <p class="px-1 text-sm text-muted">
            共 {{ nodes.length }} 个节点（令牌只写不读：仅展示已配置状态，永不回显令牌内容）
          </p>

          <UCard
            v-if="nodes.length > 0"
            :ui="{ body: 'p-0' }"
          >
            <UTable
              :data="nodes"
              :columns="[{
                accessorKey: 'name',
                header: '节点名'
              }, {
                accessorKey: 'host',
                header: '主机'
              }, {
                accessorKey: 'api_user',
                header: 'API 用户'
              }, {
                accessorKey: 'enabled',
                header: '状态'
              }, {
                accessorKey: 'api_token_set',
                header: '令牌'
              }, {
                accessorKey: 'created_at',
                header: '创建时间'
              }, {
                accessorKey: 'actions',
                header: '操作'
              }]"
            >
              <template #name-cell="{ row }">
                <div class="flex flex-col">
                  <span class="font-medium">{{ row.original.name }}</span>
                  <span class="text-xs text-muted">{{ row.original.pve_name }}</span>
                </div>
              </template>

              <template #host-cell="{ row }">
                <code class="text-sm">{{ row.original.host }}:{{ row.original.port }}</code>
              </template>

              <template #enabled-cell="{ row }">
                <UBadge
                  :label="row.original.enabled ? '启用' : '禁用'"
                  :color="row.original.enabled ? 'success' : 'neutral'"
                  variant="subtle"
                />
              </template>

              <template #api_token_set-cell="{ row }">
                <UBadge
                  :label="row.original.api_token_set ? '已配置' : '未配置'"
                  :color="row.original.api_token_set ? 'success' : 'warning'"
                  variant="subtle"
                />
              </template>

              <template #created_at-cell="{ row }">
                <span class="text-sm text-muted">{{ formatDateTime(row.original.created_at) }}</span>
              </template>

              <template #actions-cell="{ row }">
                <UButton
                  icon="i-lucide-pencil"
                  label="编辑"
                  color="neutral"
                  variant="ghost"
                  size="sm"
                  @click="openEditModal(row.original)"
                />
              </template>
            </UTable>
          </UCard>

          <AppEmpty
            v-else
            title="暂无节点"
            description="点击右上角「登记节点」将该区域的 PVE 节点纳入管理。"
            icon="i-lucide-server"
          />
        </template>
      </div>

      <!-- BUG-1：UModal 默认插槽是 DialogTrigger（as-child 内联渲染），内容放默认插槽会内联铺在页面上、
           点击提交还会连带触发打开空弹窗；因此弹窗内容必须进 #body、按钮进 #footer。
           UModal 位于 panel 的 #body 具名插槽末尾（panel 默认插槽为空，不触发插槽冲突）；
           且 UModal 默认 portal 渲染到 body，DOM 位置不影响表现，弹窗只在点击按钮后打开。
           lint 约束：vue/no-multiple-template-root 禁止多根，UModal 无法作为 panel 的兄弟根。 -->
      <UModal
        v-model:open="formOpen"
        :title="formMode === 'create' ? '登记节点' : `编辑节点 · ${editingNode?.name ?? ''}`"
        :description="formMode === 'edit' ? '修改非令牌字段后保存；令牌输入框留空表示保留原密钥。' : 'api_token 仅提交一次，登记后不可回显。'"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <UForm
            ref="formRef"
            :validate="validateForm"
            :validate-on="['blur', 'change']"
            class="space-y-4"
            @submit="onSubmitForm"
          >
            <UFormField
              name="name"
              label="节点名称"
              required
              :hint="'区域内唯一，如 node-1'"
            >
              <UInput
                v-model="form.name"
                placeholder="如 node-1"
              />
            </UFormField>

            <UFormField
              name="pve_name"
              label="PVE 集群节点名"
              :hint="'可选；留空表示沿用业务名（登记时自动探测）'"
            >
              <UInput
                v-model="form.pve_name"
                placeholder="如 pve-node-1"
              />
            </UFormField>

            <UFormField
              name="host"
              label="节点主机地址"
              required
              :hint="'可携带端口，如 10.0.0.10 或 10.0.0.10:8006'"
            >
              <UInput
                v-model="form.host"
                placeholder="如 10.0.0.10:8006"
              />
            </UFormField>

            <UFormField
              name="api_user"
              label="PVE API 用户"
              required
              :hint="'如 spark@pve'"
            >
              <UInput
                v-model="form.api_user"
                placeholder="如 spark@pve"
              />
            </UFormField>

            <UFormField
              name="api_token"
              :label="formMode === 'create' ? 'PVE API 令牌' : 'PVE API 令牌（可选）'"
              :required="formMode === 'create'"
              :hint="formMode === 'edit' ? '留空表示保留原密钥，不修改令牌' : '令牌只写不读，保存后不可回显'"
            >
              <UInput
                v-model="form.api_token"
                type="password"
                autocomplete="new-password"
                placeholder="粘贴 PVE API token secret"
              />
            </UFormField>

            <UFormField
              name="enabled"
              label="是否启用"
              :hint="'禁用的节点不参与 VM 分配与调度'"
            >
              <USwitch
                v-model="form.enabled"
                label="启用该节点"
              />
            </UFormField>

            <AppErrorAlert
              v-if="formError"
              :code="formError.code"
              :message="formError.message"
              :title="formMode === 'create' ? '登记失败' : '保存失败'"
            />
          </UForm>
        </template>

        <template #footer>
          <UButton
            label="取消"
            variant="outline"
            @click="formOpen = false"
          />
          <UButton
            :label="formMode === 'create' ? '登记' : '保存'"
            color="primary"
            :icon="formMode === 'create' ? 'i-lucide-plus' : 'i-lucide-check'"
            :loading="saving"
            @click="formRef?.submit()"
          />
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
