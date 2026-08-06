<script setup lang="ts">
// 镜像管理：列表 + 创建。
// node_images 为"节点名 → 镜像路径"对象：列表按 key 数量展示并在行内展开查看具体路径；
// 创建表单以动态"节点名 + 路径"键值对行录入（至少一组），提交时组装为对象。
import { createImage, listImages } from '~/api/images'
import type { ApiError } from '~/api/client'
import type { Image } from '~/api/types'
import type { FormValidateError } from '~/utils/format'

const toast = useToast()

// 列表状态
const loading = ref(true)
const error = ref<ApiError | null>(null)
const images = ref<Image[]>([])
const total = ref(0)
// 一次拉取上限 100（契约 limit 上限），超过时按第一页口径展示，总数以 X-Total-Count 为准
const PAGE_LIMIT = 100

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    const res = await listImages({ limit: PAGE_LIMIT })
    images.value = res.data
    total.value = res.total
  } catch (err) {
    error.value = err as ApiError
  } finally {
    loading.value = false
  }
}

onMounted(load)

// 创建表单：node_images 以动态键值对行录入
interface NodeImageRow {
  /** 行自增 id：作为 v-for 的 :key（避免用 index，增删行时保持输入状态稳定） */
  id: number
  node: string
  path: string
}

// 行自增 id 计数器
let rowIdSeq = 0

function newNodeImageRow(): NodeImageRow {
  rowIdSeq += 1
  return { id: rowIdSeq, node: '', path: '' }
}

const createOpen = ref(false)
const creating = ref(false)
const createError = ref<ApiError | null>(null)
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const createFormRef = ref<{ submit: () => Promise<void> }>()
const createForm = reactive({
  name: '',
  default_user: '',
  // 至少保留一行；提交前校验每一行必须同时填写节点名与路径（全空行视为未填写）
  nodeImages: [newNodeImageRow()] as NodeImageRow[]
})

function validateCreate(): FormValidateError[] {
  const errors: FormValidateError[] = []
  if (!createForm.name.trim()) errors.push({ name: 'name', message: '请输入镜像名' })
  if (!createForm.default_user.trim()) errors.push({ name: 'default_user', message: '请输入默认登录用户' })
  // 逐行校验：每行要么全空（视为未填写），要么节点名与路径都填，禁止部分填写（静默丢弃）
  const seenNodes = new Set<string>()
  createForm.nodeImages.forEach((row, index) => {
    const hasNode = Boolean(row.node.trim())
    const hasPath = Boolean(row.path.trim())
    if (hasNode !== hasPath) {
      errors.push({
        name: `node_images.${index}.node`,
        message: hasNode ? '该行缺少镜像路径' : '该行缺少节点名'
      })
    }
    // 重复节点名：提交时组装为"节点名→路径"对象，同名行后者覆盖前者（静默丢数据），按行拦截
    if (hasNode) {
      const node = row.node.trim()
      if (seenNodes.has(node)) {
        errors.push({ name: `node_images.${index}.node`, message: `节点名「${node}」已填写，请勿重复` })
      }
      seenNodes.add(node)
    }
  })
  const filledRows = createForm.nodeImages.filter(r => r.node.trim() && r.path.trim())
  if (filledRows.length === 0) {
    errors.push({ name: 'node_images', message: '请至少填写一组节点名与路径' })
  }
  return errors
}

function resetCreateForm(): void {
  createForm.name = ''
  createForm.default_user = ''
  createForm.nodeImages = [newNodeImageRow()]
  createError.value = null
}

function openCreateModal(): void {
  resetCreateForm()
  createOpen.value = true
}

function addNodeImageRow(): void {
  createForm.nodeImages.push(newNodeImageRow())
}

function removeNodeImageRow(index: number): void {
  // 至少保留一行，避免表单无法继续添加
  if (createForm.nodeImages.length > 1) {
    createForm.nodeImages.splice(index, 1)
  }
}

async function onSubmitCreate(): Promise<void> {
  // 组装 node_images 对象：跳过未填写的行（校验已保证至少一组完整）
  const nodeImages: Record<string, string> = {}
  for (const row of createForm.nodeImages) {
    const node = row.node.trim()
    const path = row.path.trim()
    if (node && path) nodeImages[node] = path
  }

  creating.value = true
  createError.value = null
  try {
    const createdName = createForm.name.trim()
    await createImage({
      name: createdName,
      default_user: createForm.default_user.trim(),
      node_images: nodeImages
    })
    createOpen.value = false
    resetCreateForm()
    toast.add({ title: '创建成功', description: `镜像「${createdName}」已登记`, color: 'success' })
    await load()
  } catch (err) {
    createError.value = err as ApiError
  } finally {
    creating.value = false
  }
}
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
          v-else-if="loading"
          :rows="4"
        />

        <template v-else>
          <p class="px-1 text-sm text-muted">
            共 {{ total }} 个镜像（node_images 为各节点上的镜像路径）
          </p>

          <UCard
            v-if="images.length > 0"
            :ui="{ body: 'p-0' }"
          >
            <UTable
              :data="images"
              :columns="[{
                accessorKey: 'name',
                header: '镜像名'
              }, {
                accessorKey: 'default_user',
                header: '默认用户'
              }, {
                accessorKey: 'node_images',
                header: '节点路径'
              }, {
                accessorKey: 'created_at',
                header: '创建时间'
              }]"
            >
              <template #name-cell="{ row }">
                <span class="font-medium">{{ row.original.name }}</span>
              </template>

              <template #default_user-cell="{ row }">
                <code class="text-sm">{{ row.original.default_user }}</code>
              </template>

              <template #node_images-cell="{ row }">
                <div class="flex items-center gap-2">
                  <UBadge
                    :label="`${Object.keys(row.original.node_images).length} 个节点`"
                    color="neutral"
                    variant="subtle"
                  />
                  <UCollapsible>
                    <template #trigger="{ open }">
                      <UButton
                        size="xs"
                        variant="ghost"
                        color="neutral"
                        :icon="open ? 'i-lucide-chevron-up' : 'i-lucide-chevron-down'"
                        :label="open ? '收起' : '展开路径'"
                      />
                    </template>
                    <ul class="mt-1 space-y-0.5 text-xs text-muted">
                      <li
                        v-for="(path, node) in row.original.node_images"
                        :key="node"
                      >
                        <span class="font-medium text-foreground">{{ node }}</span>
                        <code> → {{ path }}</code>
                      </li>
                    </ul>
                  </UCollapsible>
                </div>
              </template>

              <template #created_at-cell="{ row }">
                <span class="text-sm text-muted">{{ formatDateTime(row.original.created_at) }}</span>
              </template>
            </UTable>
          </UCard>

          <AppEmpty
            v-else
            title="暂无镜像"
            description="点击右上角「登记镜像」录入 cloud 镜像名与各节点路径。"
            icon="i-lucide-image"
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
        title="登记镜像"
        :description="'为每个节点指定该镜像在其上的路径；至少登记一组节点路径'"
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
              name="node_images"
              :label="`节点路径（${createForm.nodeImages.length} 组）`"
              required
              :hint="'节点名对应其上的镜像路径；可多组'"
            >
              <div class="space-y-2">
                <!-- 每行一个 UFormField：部分填写（只填了节点名或只填了路径）按行定位报错 -->
                <UFormField
                  v-for="(row, index) in createForm.nodeImages"
                  :key="row.id"
                  :name="`node_images.${index}.node`"
                  :description="'每行需同时填写节点名与路径'"
                >
                  <div class="flex items-center gap-2">
                    <UInput
                      v-model="row.node"
                      placeholder="节点名，如 node-1"
                      class="flex-1"
                    />
                    <UInput
                      v-model="row.path"
                      placeholder="镜像路径，如 /var/lib/vz/template/iso/deb.img"
                      class="flex-[2]"
                    />
                    <UButton
                      icon="i-lucide-trash-2"
                      color="error"
                      variant="ghost"
                      size="sm"
                      :disabled="createForm.nodeImages.length === 1"
                      aria-label="删除该组节点路径"
                      @click="removeNodeImageRow(index)"
                    />
                  </div>
                </UFormField>
                <UButton
                  icon="i-lucide-plus"
                  label="添加节点路径"
                  variant="outline"
                  size="sm"
                  @click="addNodeImageRow"
                />
              </div>
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
            label="登记"
            color="primary"
            icon="i-lucide-plus"
            :loading="creating"
            @click="createFormRef?.submit()"
          />
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
