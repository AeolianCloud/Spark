<script setup lang="ts">
// 存储类型管理：列表（limit/offset 分页 + X-Total-Count 总数）+ 创建/编辑 + 删除（二次确认）。
// 删除被引用（如已有 VM 使用）时后端返回 409 conflict，用 AppErrorAlert 展示后端错误。
import {
  createStorageType,
  deleteStorageType,
  getStorageType,
  listStorageTypes,
  updateStorageType
} from '~/api/storage-types'
import type { ApiError } from '~/api/client'
import type { StorageType } from '~/api/types'
import type { FormValidateError } from '~/utils/format'

const toast = useToast()

// 列表状态：分页驱动（limit/offset，每页条数取契约默认 25）
const PAGE_LIMIT = 25
const loading = ref(true)
const error = ref<ApiError | null>(null)
const items = ref<StorageType[]>([])
const total = ref(0)
const page = ref(1)

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / PAGE_LIMIT)))

// 请求序号守卫：快速翻页时丢弃过期响应（慢响应不覆盖快响应）
let loadSeq = 0

async function load(): Promise<void> {
  const seq = ++loadSeq
  loading.value = true
  error.value = null
  try {
    const res = await listStorageTypes({ limit: PAGE_LIMIT, offset: (page.value - 1) * PAGE_LIMIT })
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

onMounted(load)

// 页码变化时重新拉取对应分页数据
watch(page, () => {
  void load()
})

// 创建/编辑表单（共用）：编辑时先用 getStorageType 拉取详情预填
const formOpen = ref(false)
const formMode = ref<'create' | 'edit'>('create')
const saving = ref(false)
const formError = ref<ApiError | null>(null)
const editingItem = ref<StorageType | null>(null)
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const formRef = ref<{ submit: () => Promise<void> }>()
const form = reactive({
  name: '',
  display_name: '',
  pve_storage: ''
})

function validateForm(): FormValidateError[] {
  const errors: FormValidateError[] = []
  if (!form.name.trim()) errors.push({ name: 'name', message: '请输入存储类型名' })
  if (!form.display_name.trim()) errors.push({ name: 'display_name', message: '请输入展示名' })
  if (!form.pve_storage.trim()) errors.push({ name: 'pve_storage', message: '请输入 PVE 存储名' })
  return errors
}

function resetForm(): void {
  form.name = ''
  form.display_name = ''
  form.pve_storage = ''
  formError.value = null
  editingItem.value = null
}

function openCreateModal(): void {
  formMode.value = 'create'
  resetForm()
  formOpen.value = true
}

async function openEditModal(item: StorageType): Promise<void> {
  formMode.value = 'edit'
  resetForm()
  formOpen.value = true
  // 编辑预填：按契约从详情端点拉取，避免依赖列表行数据
  try {
    const res = await getStorageType(item.id)
    editingItem.value = res.data
    form.name = res.data.name
    form.display_name = res.data.display_name
    form.pve_storage = res.data.pve_storage
  } catch (err) {
    // 预填失败：保持弹窗打开展示错误（与提交失败口径一致，错误随 AppErrorAlert 可见），
    // 用户取消关闭后 formError 由下次 resetForm 清空
    formError.value = err as ApiError
  }
}

async function onSubmitForm(): Promise<void> {
  const payload = {
    name: form.name.trim(),
    display_name: form.display_name.trim(),
    pve_storage: form.pve_storage.trim()
  }
  saving.value = true
  formError.value = null
  try {
    if (formMode.value === 'create') {
      await createStorageType(payload)
      toast.add({ title: '创建成功', description: `存储类型「${form.name}」已登记`, color: 'success' })
    } else {
      await updateStorageType(editingItem.value!.id, payload)
      toast.add({ title: '保存成功', description: `存储类型「${form.name}」已更新`, color: 'success' })
    }
    formOpen.value = false
    resetForm()
    await load()
  } catch (err) {
    formError.value = err as ApiError
  } finally {
    saving.value = false
  }
}

// 删除：二次确认（UModal），失败（如 409 被引用）在确认框内展示后端错误
const deleteOpen = ref(false)
const deleting = ref(false)
const deleteError = ref<ApiError | null>(null)
const deletingItem = ref<StorageType | null>(null)

function openDeleteModal(item: StorageType): void {
  deletingItem.value = item
  deleteError.value = null
  deleteOpen.value = true
}

async function onConfirmDelete(): Promise<void> {
  if (!deletingItem.value) return
  deleting.value = true
  deleteError.value = null
  try {
    await deleteStorageType(deletingItem.value.id)
    deleteOpen.value = false
    toast.add({ title: '删除成功', description: `存储类型「${deletingItem.value.name}」已删除`, color: 'success' })
    // 删除后若当前页已空且非第一页，回退一页
    if (items.value.length === 1 && page.value > 1) {
      page.value -= 1
    } else {
      await load()
    }
  } catch (err) {
    deleteError.value = err as ApiError
  } finally {
    deleting.value = false
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
            icon="i-lucide-plus"
            color="primary"
            @click="openCreateModal"
          >
            登记存储类型
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
          <div class="flex items-center justify-between px-1">
            <p class="text-sm text-muted">
              共 {{ total }} 条 · 第 {{ page }}/{{ totalPages }} 页
            </p>
          </div>

          <UCard
            v-if="items.length > 0"
            :ui="{ body: 'p-0' }"
          >
            <UTable
              :data="items"
              :columns="[{
                accessorKey: 'name',
                header: '名称'
              }, {
                accessorKey: 'display_name',
                header: '展示名'
              }, {
                accessorKey: 'pve_storage',
                header: 'PVE 存储'
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

              <template #pve_storage-cell="{ row }">
                <code class="text-sm">{{ row.original.pve_storage }}</code>
              </template>

              <template #created_at-cell="{ row }">
                <span class="text-sm text-muted">{{ formatDateTime(row.original.created_at) }}</span>
              </template>

              <template #actions-cell="{ row }">
                <div class="flex items-center gap-1">
                  <UButton
                    icon="i-lucide-pencil"
                    label="编辑"
                    color="neutral"
                    variant="ghost"
                    size="sm"
                    @click="openEditModal(row.original)"
                  />
                  <UButton
                    icon="i-lucide-trash-2"
                    label="删除"
                    color="error"
                    variant="ghost"
                    size="sm"
                    @click="openDeleteModal(row.original)"
                  />
                </div>
              </template>
            </UTable>
          </UCard>

          <AppEmpty
            v-else
            title="暂无存储类型"
            description="点击右上角「登记存储类型」录入 PVE 真实存储。"
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
        v-model:open="formOpen"
        :title="formMode === 'create' ? '登记存储类型' : '编辑存储类型'"
        :description="'name 为对外抽象名（唯一，如 ssd）；pve_storage 为真实 PVE 存储名（如 local-ssd）'"
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
              label="名称"
              required
              :hint="'对外抽象名，全局唯一，如 ssd'"
            >
              <UInput
                v-model="form.name"
                placeholder="如 ssd"
              />
            </UFormField>

            <UFormField
              name="display_name"
              label="展示名"
              required
            >
              <UInput
                v-model="form.display_name"
                placeholder="如 固态硬盘"
              />
            </UFormField>

            <UFormField
              name="pve_storage"
              label="PVE 存储名"
              required
              :hint="'PVE 节点上的真实存储名，如 local-ssd'"
            >
              <UInput
                v-model="form.pve_storage"
                placeholder="如 local-ssd"
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

      <UModal
        v-model:open="deleteOpen"
        title="删除存储类型"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <div class="space-y-4">
            <UAlert
              color="warning"
              variant="subtle"
              icon="i-lucide-triangle-alert"
              :title="`确定删除「${deletingItem?.name ?? ''}」吗？`"
              :description="'删除后不可恢复；若已被虚拟机引用，后端将返回冲突错误且不会删除。'"
            />

            <AppErrorAlert
              v-if="deleteError"
              :code="deleteError.code"
              :message="deleteError.message"
              title="删除失败"
            />
          </div>
        </template>

        <template #footer>
          <UButton
            label="取消"
            variant="outline"
            @click="deleteOpen = false"
          />
          <UButton
            label="确认删除"
            color="error"
            icon="i-lucide-trash-2"
            :loading="deleting"
            @click="onConfirmDelete"
          />
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
