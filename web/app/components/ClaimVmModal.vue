<script setup lang="ts">
/**
 * 认领外部 VM 弹窗（列表页 external 行与详情页共用）：
 * - 目标节点/PVE VMID 取自 external 条目（只读），表单仅需确认 zone + 可选 IP/名称；
 *   IP 不传则不分配（网络由 PVE 侧配置决定）
 * - 可用区选项弹窗内懒加载（与页面级目录缓存无关），节点所属可用区可预填
 * - 认领成功：弹窗内 toast 提示并 emit claimed/close，由父页面决定刷新或跳转
 */
import type { VMListItem, ZoneResponse } from '~/api'
import { ApiError, importVM, listZones } from '~/api'
import { useCatalog } from '~/composables/useCatalog'

const props = defineProps<{
  /** 认领目标（external 条目）；null 时弹窗关闭 */
  vm: VMListItem | null
}>()

const emit = defineEmits<{
  /** 关闭弹窗（父页面将 vm prop 置回 null） */
  close: []
  /** 认领成功（父页面刷新列表/详情，external 条目转为 claimed） */
  claimed: []
}>()

const toast = useToast()
const { nodeName, zoneIdOfNode } = useCatalog()

// 弹窗开关：以 vm prop 为准（setter 处理各类关闭路径）
const open = computed({
  get: () => props.vm !== null,
  set: (v: boolean) => {
    if (!v) emit('close')
  }
})

const claiming = ref(false)
const claimError = ref<ApiError | null>(null)
// 表单实例引用：footer 提交按钮经由 form.submit() 触发校验与 @submit
const claimFormRef = ref<{ submit: () => Promise<void> }>()

// 表单状态：zone 必选；ip/name 可选
const claimForm = reactive({
  zone_id: undefined as number | undefined,
  ip: '',
  name: ''
})

// 可用区选项：弹窗内懒加载并缓存（重新打开不重复请求；失败记录错误 ref 弹窗内轻量提示）
const zones = ref<ZoneResponse[]>([])
const zonesLoadError = ref<ApiError | null>(null)

async function loadZones(): Promise<void> {
  if (zones.value.length > 0) return
  zonesLoadError.value = null
  try {
    zones.value = (await listZones({ limit: 100 })).data
  } catch (err) {
    zonesLoadError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
    zones.value = []
  }
}

const zoneOptions = computed(() => zones.value.map(z => ({ label: z.name, value: z.id })))

// 打开弹窗：表单重置 + 可用区懒加载；目录映射就绪时按节点预填可用区（节点必属某可用区）
watch(() => props.vm, async (vm) => {
  if (!vm) return
  claimError.value = null
  claimForm.zone_id = zoneIdOfNode(vm.node_id) ?? undefined
  claimForm.ip = ''
  claimForm.name = ''
  await loadZones()
  // 预填值双源校验：预填值来自 catalog 缓存，下拉选项来自本组件 zones；
  // catalog 过期或 loadZones 失败时预填值不在 zoneOptions 中，置空走"请选择可用区"校验路径
  if (claimForm.zone_id !== undefined && !zoneOptions.value.some(opt => opt.value === claimForm.zone_id)) {
    claimForm.zone_id = undefined
  }
})

// 表单校验：可用区必选；IP 非空时须为 IPv4/IPv6 形式（宽松预检，格式细节由后端 400 兜底）；
// 名称上限 128 字符与后端契约 ImportVMRequest.name maxLength 对齐，字符集须匹配契约
// pattern ^[A-Za-z0-9_][A-Za-z0-9_.\-]*$（留空不校验，后端按 PVE 配置名兜底）
const VM_NAME_PATTERN = /^[A-Za-z0-9_][A-Za-z0-9_.-]*$/

/** IP 宽松预检（后端严格兜底）：IPv4 每段 0-255；IPv6 须含冒号且含数字（排除纯字母串如 abc） */
function isValidIp(ip: string): boolean {
  // IPv4：4 段十进制，每段值不得超过 255
  if (/^(\d{1,3}\.){3}\d{1,3}$/.test(ip)) {
    return ip.split('.').every(seg => Number(seg) <= 255)
  }
  // IPv6：字符集限定 hex + 冒号，且须含冒号与至少一个数字（纯字母串明显非 IP）
  return /^[0-9a-fA-F:]+$/.test(ip) && ip.includes(':') && /\d/.test(ip)
}

function validateClaimForm(): { name?: string, message: string }[] {
  const errors: { name?: string, message: string }[] = []
  if (claimForm.zone_id === undefined) errors.push({ name: 'zone_id', message: '请选择可用区' })
  const ip = claimForm.ip.trim()
  if (ip && !isValidIp(ip)) errors.push({ name: 'ip', message: 'IP 格式非法（仅支持 IPv4/IPv6）' })
  const name = claimForm.name.trim()
  if (name.length > 128) errors.push({ name: 'name', message: '名称最多 128 字符' })
  else if (name && !VM_NAME_PATTERN.test(name)) errors.push({ name: 'name', message: '名称须以字母、数字或下划线开头，后续可含 . 与 -' })
  return errors
}

// 提交认领：zone 必传（校验保证非空）；node/pve_vmid 取自 external 条目；ip/name 留空不传
async function submitClaim(): Promise<void> {
  const target = props.vm
  if (!target || claiming.value) return
  // 防御性二次校验（UForm 校验之外的最后防线）：不通过则不发起请求
  const residualErrors = validateClaimForm()
  if (residualErrors.length > 0) {
    claimError.value = new ApiError(400, 'bad_request', residualErrors.map(e => e.message).join('；'))
    return
  }
  const pveVmid = target.pve_vmid
  if (pveVmid === undefined) {
    // external 条目按契约恒携带 pve_vmid；此处防御异常数据，避免以 undefined 发请求
    claimError.value = new ApiError(400, 'bad_request', '该虚拟机缺少 PVE VMID，无法认领')
    return
  }
  claiming.value = true
  claimError.value = null
  try {
    // 校验已保证 zone_id 非空；external 条目 node_id 恒存在
    await importVM({
      zone_id: claimForm.zone_id!,
      node_id: target.node_id,
      pve_vmid: pveVmid,
      ip: claimForm.ip.trim() || undefined,
      name: claimForm.name.trim() || undefined
    })
    // 成功后 toast 提示并通知父页面（关闭弹窗 + 刷新，external 条目转为 claimed）
    toast.add({ title: '已认领', description: `「${target.name}」已认领为托管虚拟机`, color: 'success', icon: 'i-lucide-check-circle-2' })
    emit('claimed')
    emit('close')
  } catch (err) {
    // 契约错误（含 vm_already_managed / vm_not_found_on_node / ip_exhausted 等）由 AppErrorAlert 展示错误码与后端描述
    claimError.value = err instanceof ApiError ? err : new ApiError(0, 'unknown', err instanceof Error ? err.message : '未知错误')
  } finally {
    claiming.value = false
  }
}
</script>

<template>
  <!-- BUG-1：UModal 默认插槽是 DialogTrigger（as-child 内联渲染），内容必须进 #body、按钮进 #footer。
       认领基于 external 条目，节点/PVE VMID 只读展示，zone + 可选 IP/名称，无密码字段 -->
  <UModal
    v-model:open="open"
    title="认领虚拟机"
    description="将 PVE 上的外部虚拟机认领为本平台托管虚拟机（不修改 PVE 侧配置）"
    :dismissible="!claiming"
    :ui="{ footer: 'justify-end' }"
  >
    <template #body>
      <UForm
        ref="claimFormRef"
        :state="claimForm"
        :validate="validateClaimForm"
        class="space-y-4"
        @submit="submitClaim"
      >
        <!-- 认领目标信息（只读）：来自 external 条目，认领请求固定指向该节点/PVE VMID -->
        <div class="rounded-md border border-dashed border-muted px-3 py-2 text-sm">
          <div class="flex flex-wrap gap-x-6 gap-y-1">
            <span>
              <span class="text-muted">目标：</span>
              {{ vm?.name }}
            </span>
            <span>
              <span class="text-muted">节点：</span>
              {{ vm ? nodeName(vm.node_id) ?? `节点 #${vm.node_id}（名称未加载）` : '—' }}
            </span>
            <span>
              <span class="text-muted">PVE VMID：</span>
              {{ vm?.pve_vmid ?? '—' }}
            </span>
          </div>
        </div>

        <UFormField
          name="zone_id"
          label="可用区"
          required
          :description="zonesLoadError
            ? `可用区列表加载失败（${zonesLoadError.code}），请关闭弹窗后重新打开重试`
            : (claimForm.zone_id === undefined ? '节点所在可用区未知，请选择' : '已按节点所在可用区预填，可修改')"
        >
          <USelect
            :model-value="claimForm.zone_id"
            :items="zoneOptions"
            placeholder="选择认领到哪个可用区"
            class="w-full"
            @update:model-value="(v: number | undefined) => { claimForm.zone_id = v }"
          />
        </UFormField>

        <UFormField
          name="ip"
          label="IP 地址"
          description="可选；不填则不分配 IP，虚拟机网络由 PVE 侧配置决定"
        >
          <UInput
            v-model="claimForm.ip"
            maxlength="45"
            placeholder="不填则不分配 IP"
            autocomplete="off"
          />
        </UFormField>

        <UFormField
          name="name"
          label="名称"
          description="可选；留空使用 PVE 上的名称"
        >
          <UInput
            v-model="claimForm.name"
            maxlength="128"
            placeholder="留空使用 PVE 上的名称"
            autocomplete="off"
          />
        </UFormField>
      </UForm>

      <!-- 认领失败（含 vm_already_managed / vm_not_found_on_node / ip_exhausted 等契约错误码） -->
      <AppErrorAlert
        v-if="claimError"
        class="mt-4"
        :code="claimError.code"
        :message="claimError.message"
        title="认领失败"
      />
    </template>

    <template #footer>
      <UButton
        variant="outline"
        :disabled="claiming"
        @click="emit('close')"
      >
        取消
      </UButton>
      <UButton
        color="primary"
        :loading="claiming"
        @click="claimFormRef?.submit()"
      >
        认领
      </UButton>
    </template>
  </UModal>
</template>
