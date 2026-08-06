<script setup lang="ts">
/**
 * VM 详情页（4.3/4.5）：
 * - 信息区：名称/状态/UUID/PVE VMID/IP/规格/所属 Zone 与节点/创建更新时间/provision_error
 * - 实时状态区：cpu_usage/内存/磁盘/uptime（穿透字段缺省表示停机或无 PVE 对端）
 *   PVE 不可达时 getVM 返回 503 node_unavailable，展示降级提示而非伪造状态
 * - 生命周期操作：启动/停止/重启（202 受理即成功）、调整规格（部分更新语义，磁盘只增）、
 *   销毁（二次确认）；失败均展示后端错误且状态不变
 */
import type { ResizeRequest, VMListItem } from '~/api'
import { ApiError, destroyVM, getVM, resizeVM, restartVM, startVM, stopVM } from '~/api'
import { useCatalog } from '~/composables/useCatalog'
import { formatDateTime } from '~/utils/format'
import {
  canResizeVM,
  formatBytes,
  formatMemMB,
  formatUptime
} from '~/utils/vm'

const route = useRoute()
const router = useRouter()
const toast = useToast()
const { refresh: refreshCatalog, zoneName, nodeName } = useCatalog()

// 路由参数：/vms/:id；非法值（NaN/非正整数）防御：不向 API 发请求（与 zones/[zoneId]/nodes.vue 模式一致）
const vmId = computed(() => {
  const n = Number(route.params.id)
  return Number.isInteger(n) && n > 0 ? n : 0
})

// ---- 详情数据 ----
const vm = ref<VMListItem | null>(null)
const loading = ref(true)
const error = ref<ApiError | null>(null)

// 请求序号守卫：快速切换路由（组件复用）时丢弃过期响应，防止旧 VM 数据覆盖新 VM
let fetchSeq = 0

async function fetchVM(): Promise<void> {
  const id = vmId.value
  const seq = ++fetchSeq
  loading.value = true
  error.value = null
  try {
    // 路由参数非法：直接展示错误，避免以 NaN/0 发起请求
    if (!id) {
      error.value = new ApiError(400, 'bad_request', 'VM ID 不合法')
      return
    }
    const res = await getVM(id)
    // 过期响应丢弃：期间已发起更新的请求，或路由已切换到其他 VM
    if (seq !== fetchSeq || id !== vmId.value) return
    vm.value = res.data
  } catch (err) {
    if (seq !== fetchSeq || id !== vmId.value) return
    // 刷新失败保留旧数据，仅展示错误条（首次加载无数据时自然展示错误态）
    error.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    if (seq === fetchSeq) loading.value = false
  }
}

// 路由参数变化（如从列表进入另一台 VM 时组件复用）→ 清空旧数据并重新拉取
watch(() => route.params.id, () => {
  vm.value = null
  error.value = null
  void fetchVM()
  // 目录映射随 VM 切换一并刷新（Zone/节点变更后名称映射不长期过期）
  void refreshCatalog()
})

onMounted(() => {
  void fetchVM()
  // 目录映射刷新（失败不阻塞详情，映射缺失时展示 id 并注明）：进入页面即重拉
  void refreshCatalog()
})

// ---- 实时状态（穿透字段均为可选：停机或无 PVE 对端时缺省）----
// 各字段独立判定：任一存在即展示实时状态卡片，缺失的条目单独回落为占位（不伪造"0% 进度条"）
const hasCpu = computed(() => vm.value?.cpu_usage !== undefined)
const hasMem = computed(() => vm.value?.mem !== undefined && vm.value.maxmem !== undefined)
const hasDisk = computed(() => vm.value?.disk !== undefined && vm.value.maxdisk !== undefined)
const hasUptime = computed(() => vm.value?.uptime !== undefined)
const hasRealtime = computed(() => Boolean(vm.value && (hasCpu.value || hasMem.value || hasDisk.value || hasUptime.value)))

function clampPercent(n: number): number {
  if (!Number.isFinite(n)) return 0
  return Math.min(100, Math.max(0, Math.round(n)))
}

// cpu_usage 为 0-1 比例（契约语义，如 0.25 = 25%），展示为百分比前必须 ×100
const cpuPercent = computed(() => clampPercent((vm.value?.cpu_usage ?? 0) * 100))
const memPercent = computed(() => {
  const v = vm.value
  if (!v || v.mem === undefined || v.maxmem === undefined) return 0
  return clampPercent((v.mem / v.maxmem) * 100)
})
const diskPercent = computed(() => {
  const v = vm.value
  if (!v || v.disk === undefined || v.maxdisk === undefined) return 0
  return clampPercent((v.disk / v.maxdisk) * 100)
})

// ---- 生命周期操作（4.5）：202 受理即成功；失败展示后端错误且状态不变 ----
const actionBusy = ref(false)
const actionError = ref<ApiError | null>(null)

const ACTION_LABELS = { start: '启动', stop: '关闭', restart: '重启' } as const
type LifecycleAction = keyof typeof ACTION_LABELS

async function runLifecycle(action: LifecycleAction): Promise<void> {
  if (!vm.value || actionBusy.value) return
  actionBusy.value = true
  actionError.value = null
  try {
    const fn = action === 'start' ? startVM : action === 'stop' ? stopVM : restartVM
    await fn(vm.value.id)
    toast.add({
      title: '操作已受理',
      description: `已提交${ACTION_LABELS[action]}，异步生效，稍后可点击刷新查看最新状态`,
      color: 'success',
      icon: 'i-lucide-check-circle-2'
    })
    void fetchVM()
  } catch (err) {
    actionError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    actionBusy.value = false
  }
}

// ---- 调整规格（4.5）：部分更新语义，仅提交有变化的字段；磁盘只增，缩小提交前即提示 ----
const resizeOpen = ref(false)
const resizing = ref(false)
const resizeError = ref<ApiError | null>(null)
const resizeForm = reactive({ cpu: 1, mem_mb: 1, disk_gb: 1 })
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const resizeFormRef = ref<{ submit: () => Promise<void> }>()

function openResize(): void {
  if (!vm.value) return
  resizeForm.cpu = vm.value.cpu
  resizeForm.mem_mb = vm.value.mem_mb
  resizeForm.disk_gb = vm.value.disk_gb
  resizeError.value = null
  resizeOpen.value = true
}

// 磁盘缩小预检：输入小于当前值时提交前即提示（后端另有 422 disk_shrink_not_allowed 兜底）
const diskShrink = computed(() => vm.value !== null && resizeForm.disk_gb < vm.value.disk_gb)

function validateResize(): { name?: string, message: string }[] {
  const errors: { name?: string, message: string }[] = []
  if (!Number.isInteger(resizeForm.cpu) || resizeForm.cpu < 1) errors.push({ name: 'cpu', message: 'vCPU 至少为 1' })
  if (!Number.isInteger(resizeForm.mem_mb) || resizeForm.mem_mb < 1) errors.push({ name: 'mem_mb', message: '内存至少为 1 MB' })
  if (!Number.isInteger(resizeForm.disk_gb) || resizeForm.disk_gb < 1) errors.push({ name: 'disk_gb', message: '磁盘至少为 1 GB' })
  if (diskShrink.value) errors.push({ name: 'disk_gb', message: '磁盘只增不减，不能缩小' })
  return errors
}

async function submitResize(): Promise<void> {
  if (!vm.value || resizing.value) return
  // 部分更新：仅提交有变化的字段（JSON Merge Patch 语义）
  const body: ResizeRequest = {}
  if (resizeForm.cpu !== vm.value.cpu) body.cpu = resizeForm.cpu
  if (resizeForm.mem_mb !== vm.value.mem_mb) body.mem_mb = resizeForm.mem_mb
  if (resizeForm.disk_gb !== vm.value.disk_gb) body.disk_gb = resizeForm.disk_gb
  if (Object.keys(body).length === 0) {
    resizeOpen.value = false
    return
  }
  resizing.value = true
  resizeError.value = null
  try {
    await resizeVM(vmId.value, body)
    resizeOpen.value = false
    toast.add({ title: '规格已更新', description: '调整规格请求已生效', color: 'success', icon: 'i-lucide-check-circle-2' })
    void fetchVM()
  } catch (err) {
    // 422 disk_shrink_not_allowed 等契约错误由 AppErrorAlert 展示错误码与后端描述
    resizeError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    resizing.value = false
  }
}

// ---- 销毁（4.5）：必须二次确认（输入 VM 名称，警示不可恢复）----
const destroyOpen = ref(false)
const destroyInput = ref('')
const destroyBusy = ref(false)
const destroyError = ref<ApiError | null>(null)

const destroyConfirmDisabled = computed(() => destroyInput.value !== vm.value?.name)

function openDestroy(): void {
  destroyInput.value = ''
  destroyError.value = null
  destroyOpen.value = true
}

async function confirmDestroy(): Promise<void> {
  if (!vm.value || destroyConfirmDisabled.value || destroyBusy.value) return
  destroyBusy.value = true
  destroyError.value = null
  try {
    await destroyVM(vm.value.id)
    toast.add({ title: '已销毁', description: `虚拟机「${vm.value.name}」已删除`, color: 'success', icon: 'i-lucide-trash-2' })
    destroyOpen.value = false
    // 销毁完成后返回列表
    void router.push('/vms')
  } catch (err) {
    destroyError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    destroyBusy.value = false
  }
}
</script>

<template>
  <UDashboardPanel>
    <template #header>
      <UDashboardNavbar :title="vm ? vm.name : 'VM 详情'">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #trailing>
          <UButton
            v-if="vm"
            icon="i-lucide-refresh-cw"
            variant="ghost"
            :loading="loading"
            @click="fetchVM"
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
          to="/vms"
        >
          返回列表
        </UButton>

        <!-- 加载态骨架屏：仅首次加载（无数据）时显示；手动刷新保留旧数据，避免闪烁 -->
        <AppLoading
          v-if="!vm && loading"
          :rows="6"
        />

        <template v-else>
          <!-- 加载失败：503 node_unavailable 等，展示降级提示不伪造状态 -->
          <AppErrorAlert
            v-if="error"
            :code="error.code"
            :message="error.message"
            title="详情加载失败"
          />

          <template v-else-if="vm">
            <!-- 供给失败提示 -->
            <UAlert
              v-if="vm.provision_error"
              color="error"
              variant="subtle"
              icon="i-lucide-circle-alert"
              title="供给失败"
              :description="vm.provision_error"
            />

            <!-- 生命周期操作区：creating/failed 状态禁用（后端 vm_not_ready 兜底） -->
            <VmActions
              :status="vm.status"
              :busy="actionBusy"
              :busy-any="actionBusy"
              :show-resize="canResizeVM(vm.status)"
              variant="soft"
              show-badge
              @start="runLifecycle('start')"
              @stop="runLifecycle('stop')"
              @restart="runLifecycle('restart')"
              @resize="openResize"
              @destroy="openDestroy"
            />

            <!-- 操作失败：展示后端错误，VM 状态不变 -->
            <AppErrorAlert
              v-if="actionError"
              :code="actionError.code"
              :message="actionError.message"
              title="操作失败"
            />

            <!-- 基本信息 -->
            <UCard>
              <template #header>
                基本信息
              </template>
              <div class="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-3">
                <div>
                  <div class="text-xs text-muted">
                    UUID
                  </div>
                  <div class="font-mono text-sm">
                    {{ vm.uuid }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    PVE VMID
                  </div>
                  <div class="text-sm">
                    {{ vm.pve_vmid ?? '—' }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    IP 地址
                  </div>
                  <div class="text-sm">
                    {{ vm.ip ?? '—' }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    vCPU
                  </div>
                  <div class="text-sm">
                    {{ vm.cpu }} 核
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    内存
                  </div>
                  <div class="text-sm">
                    {{ formatMemMB(vm.mem_mb) }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    磁盘
                  </div>
                  <div class="text-sm">
                    {{ vm.disk_gb }} GB
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    可用区
                  </div>
                  <div class="text-sm">
                    {{ zoneName(vm.zone_id) ?? `Zone #${vm.zone_id}（名称未加载）` }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    节点
                  </div>
                  <div class="text-sm">
                    {{ nodeName(vm.node_id) ?? `节点 #${vm.node_id}（名称未加载）` }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    创建时间
                  </div>
                  <div class="text-sm">
                    {{ formatDateTime(vm.created_at) }}
                  </div>
                </div>
                <div>
                  <div class="text-xs text-muted">
                    更新时间
                  </div>
                  <div class="text-sm">
                    {{ formatDateTime(vm.updated_at) }}
                  </div>
                </div>
              </div>
            </UCard>

            <!-- 实时状态（PVE 穿透） -->
            <UCard>
              <template #header>
                <div class="flex items-center justify-between">
                  <span>实时状态</span>
                  <span
                    v-if="!hasRealtime"
                    class="text-xs text-muted"
                  >VM 未运行或无 PVE 对端，实时数据不可用</span>
                </div>
              </template>
              <div
                v-if="hasRealtime"
                class="space-y-4"
              >
                <div v-if="hasCpu">
                  <div class="mb-1 flex justify-between text-sm">
                    <span>CPU 使用率</span>
                    <span>{{ cpuPercent }}%</span>
                  </div>
                  <UProgress
                    :model-value="cpuPercent"
                    size="sm"
                    color="success"
                  />
                </div>
                <div v-if="hasMem">
                  <div class="mb-1 flex justify-between text-sm">
                    <span>内存</span>
                    <span>{{ formatBytes(vm.mem) }} / {{ formatBytes(vm.maxmem) }}</span>
                  </div>
                  <UProgress
                    :model-value="memPercent"
                    size="sm"
                    color="info"
                  />
                </div>
                <div v-else>
                  <div class="flex justify-between text-sm">
                    <span>内存</span>
                    <span class="text-muted">— / —（无 PVE 对端数据）</span>
                  </div>
                </div>
                <div v-if="hasDisk">
                  <div class="mb-1 flex justify-between text-sm">
                    <span>磁盘</span>
                    <span>{{ formatBytes(vm.disk) }} / {{ formatBytes(vm.maxdisk) }}</span>
                  </div>
                  <UProgress
                    :model-value="diskPercent"
                    size="sm"
                    color="warning"
                  />
                </div>
                <div v-else>
                  <div class="flex justify-between text-sm">
                    <span>磁盘</span>
                    <span class="text-muted">— / —（无 PVE 对端数据）</span>
                  </div>
                </div>
                <div
                  v-if="hasUptime"
                  class="flex justify-between text-sm"
                >
                  <span>运行时长</span>
                  <span>{{ formatUptime(vm.uptime) }}</span>
                </div>
              </div>
              <AppEmpty
                v-else
                title="实时状态不可用"
                description="VM 处于停机状态或 PVE 对端不可达，实时数据为空。"
                icon="i-lucide-activity"
              />
            </UCard>
          </template>
        </template>
      </div>

      <!-- BUG-1：UModal 默认插槽是 DialogTrigger（as-child 内联渲染），内容放默认插槽会内联铺在页面上、
           点击提交还会连带触发打开空弹窗；因此弹窗内容必须进 #body、按钮进 #footer。
           UModal 位于 panel 的 #body 具名插槽末尾（panel 默认插槽为空，不触发插槽冲突）；
           且 UModal 默认 portal 渲染到 body，DOM 位置不影响表现，弹窗只在点击按钮后打开。
           lint 约束：vue/no-multiple-template-root 禁止多根，UModal 无法作为 panel 的兄弟根。
           BUG-3：v-model 改为 v-model:open（UModal 模型为 open/update:open） -->
      <UModal
        v-model:open="resizeOpen"
        title="调整规格"
        description="仅提交有变化的字段；磁盘只增不减"
        :dismissible="!resizing"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <UForm
            ref="resizeFormRef"
            :state="resizeForm"
            :validate="validateResize"
            class="space-y-4"
            @submit="submitResize"
          >
            <UFormField
              name="cpu"
              label="vCPU 核数"
              required
            >
              <UInputNumber
                :model-value="resizeForm.cpu"
                :min="1"
                :step="1"
                class="w-full"
                @update:model-value="(v) => { resizeForm.cpu = v ?? 1 }"
              />
            </UFormField>
            <UFormField
              name="mem_mb"
              label="内存（MB）"
              required
            >
              <UInputNumber
                :model-value="resizeForm.mem_mb"
                :min="1"
                :step="1024"
                class="w-full"
                @update:model-value="(v) => { resizeForm.mem_mb = v ?? 1 }"
              />
            </UFormField>
            <UFormField
              name="disk_gb"
              label="磁盘（GB）"
              required
              :error="diskShrink ? '磁盘只增不减，不能缩小' : undefined"
            >
              <UInputNumber
                :model-value="resizeForm.disk_gb"
                :min="1"
                :step="1"
                class="w-full"
                @update:model-value="(v) => { resizeForm.disk_gb = v ?? 1 }"
              />
            </UFormField>
          </UForm>
          <!-- 调整规格失败（422 disk_shrink_not_allowed 等契约错误码） -->
          <AppErrorAlert
            v-if="resizeError"
            class="mt-4"
            :code="resizeError.code"
            :message="resizeError.message"
            title="调整规格失败"
          />
        </template>

        <template #footer>
          <UButton
            variant="outline"
            :disabled="resizing"
            @click="resizeOpen = false"
          >
            取消
          </UButton>
          <UButton
            color="primary"
            :loading="resizing"
            @click="resizeFormRef?.submit()"
          >
            提交
          </UButton>
        </template>
      </UModal>

      <UModal
        v-model:open="destroyOpen"
        title="销毁虚拟机"
        description="该操作不可恢复，请输入虚拟机名称确认"
        :dismissible="!destroyBusy"
        :ui="{ footer: 'justify-end' }"
      >
        <template #body>
          <div class="space-y-4">
            <UAlert
              color="error"
              variant="subtle"
              icon="i-lucide-trash-2"
              title="不可恢复"
              description="虚拟机及其磁盘数据将被永久删除（含 PVE 侧 purge），无法恢复。"
            />
            <UInput
              v-model="destroyInput"
              :placeholder="`输入「${vm?.name ?? ''}」以确认`"
              autocomplete="off"
            />
            <AppErrorAlert
              v-if="destroyError"
              :code="destroyError.code"
              :message="destroyError.message"
              title="销毁失败"
            />
          </div>
        </template>

        <template #footer>
          <UButton
            variant="outline"
            :disabled="destroyBusy"
            @click="destroyOpen = false"
          >
            取消
          </UButton>
          <UButton
            color="error"
            :loading="destroyBusy"
            :disabled="destroyConfirmDisabled"
            @click="confirmDestroy"
          >
            确认销毁
          </UButton>
        </template>
      </UModal>
    </template>
  </UDashboardPanel>
</template>
