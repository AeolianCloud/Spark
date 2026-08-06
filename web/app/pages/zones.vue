<script setup lang="ts">
// 可用区管理：列表 + 创建。
// 契约缺口：docs/openapi.yaml 的 /zones 仅定义 POST（创建）与 GET（列表），
// 无 DELETE 端点，故本页不实现删除功能（含删除冲突错误展示，均不适用）。
import { createZone, listZones } from '~/api/zones'
import type { ApiError } from '~/api/client'
import type { ZoneResponse } from '~/api/types'
import type { FormValidateError } from '~/utils/format'

const route = useRoute()
const toast = useToast()

// BUG-2 修复：本页存在子路由 /zones/:zoneId/nodes，父页面只在自身路由下渲染列表，
// 子路由访问时仅渲染子页面（NuxtPage 出口），避免两个 UDashboardPanel 堆叠
const isParentRoute = computed(() => !route.path.startsWith('/zones/'))

// 列表状态
const loading = ref(true)
const error = ref<ApiError | null>(null)
const zones = ref<ZoneResponse[]>([])
const total = ref(0)
// 列表一次拉取上限 100（契约 limit 上限）；若 Zone 数超过 100 可再按 offset 翻页，此处按
// 常规规模一次性展示，总数以 X-Total-Count 为准
const PAGE_LIMIT = 100

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    const res = await listZones({ limit: PAGE_LIMIT })
    zones.value = res.data
    total.value = res.total
  } catch (err) {
    error.value = err as ApiError
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  // 子路由访问时父页面不加载列表（列表区域不渲染，避免无谓请求）
  if (isParentRoute.value) void load()
})

// 从子路由（/zones/:zoneId/nodes）返回本页时刷新：父页面常驻不重挂载（onMounted 不触发），
// 否则子页登记节点后返回，"节点数"徽章保持陈旧；与 vms.vue 的 path watch 模式一致，
// path 变化与列表加载无并发冲突（zones 列表无分页，无需序号守卫）
watch(() => route.path, (_path, oldPath) => {
  if (oldPath && oldPath.startsWith('/zones/') && isParentRoute.value) {
    void load()
  }
})

// 创建表单状态
const createOpen = ref(false)
const creating = ref(false)
const createError = ref<ApiError | null>(null)
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const createFormRef = ref<{ submit: () => Promise<void> }>()
const createForm = reactive({
  name: ''
})

function validateCreate(): FormValidateError[] {
  const errors: FormValidateError[] = []
  if (!createForm.name.trim()) {
    errors.push({ name: 'name', message: '请输入区域名称' })
  }
  return errors
}

function resetCreateForm(): void {
  createForm.name = ''
  createError.value = null
}

function openCreateModal(): void {
  resetCreateForm()
  createOpen.value = true
}

async function onSubmitCreate(): Promise<void> {
  creating.value = true
  createError.value = null
  const createdName = createForm.name.trim()
  try {
    await createZone({ name: createdName })
    createOpen.value = false
    resetCreateForm()
    toast.add({ title: '创建成功', description: `可用区「${createdName}」已创建`, color: 'success' })
    await load()
  } catch (err) {
    createError.value = err as ApiError
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <UDashboardPanel v-if="isParentRoute">
    <template #header>
      <UDashboardNavbar title="可用区">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #trailing>
          <UButton
            icon="i-lucide-plus"
            color="primary"
            @click="openCreateModal"
          >
            创建可用区
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
          title="可用区列表加载失败"
        />

        <AppLoading
          v-else-if="loading"
          :rows="4"
        />

        <template v-else>
          <div class="flex items-center justify-between px-1">
            <p class="text-sm text-muted">
              共 {{ total }} 个可用区
            </p>
          </div>

          <UCard
            v-if="zones.length > 0"
            :ui="{ body: 'p-0' }"
          >
            <UTable
              :data="zones"
              :columns="[{
                accessorKey: 'name',
                header: '区域名'
              }, {
                accessorKey: 'nodes',
                header: '节点数'
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

              <template #nodes-cell="{ row }">
                <UBadge
                  :label="String(row.original.nodes.length)"
                  color="neutral"
                  variant="subtle"
                />
              </template>

              <template #created_at-cell="{ row }">
                <span class="text-sm text-muted">{{ formatDateTime(row.original.created_at) }}</span>
              </template>

              <template #actions-cell="{ row }">
                <UButton
                  icon="i-lucide-server"
                  label="查看节点"
                  color="primary"
                  variant="ghost"
                  size="sm"
                  :to="`/zones/${row.original.id}/nodes`"
                />
              </template>
            </UTable>
          </UCard>

          <AppEmpty
            v-else
            title="暂无可用区"
            description="点击右上角「创建可用区」登记第一个区域。"
            icon="i-lucide-boxes"
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
        title="创建可用区"
        :description="`名称将作为区域唯一标识，如 cn-north-1（共 ${total} 个可用区）`"
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
              label="区域名称"
              required
              :hint="`创建后不可修改（契约无更新/删除端点）`"
            >
              <UInput
                v-model="createForm.name"
                placeholder="如 cn-north-1"
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
    </template>
  </UDashboardPanel>

  <NuxtPage v-else />
</template>
