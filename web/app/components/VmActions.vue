<script setup lang="ts">
// 生命周期操作按钮组：VM 列表页与详情页共用，仅负责按状态渲染按钮；
// 实际请求、toast 与错误展示由父组件处理（两页的 busy/错误语义不同）。
// 按钮恒显口径（design D4）：启动/关闭/重启/销毁在任何状态下数量一致，
// 不可用的操作以禁用态呈现（disabled = busy || busyAny || !canXxx(status)），
// 仅触发中的操作按钮转圈（pendingAction 定向 loading）。
import type { PendingAction } from '~/utils/vm'
import {
  canDestroyVM,
  canResizeVM,
  canRestartVM,
  canStartVM,
  canStopVM,
  vmPendingStatusBadge,
  vmStatusBadge
} from '~/utils/vm'

const props = withDefaults(defineProps<{
  /** VM 状态：决定哪些操作可用 */
  status: string
  /** 本组操作是否进行中（仅本按钮组 loading，如列表页按行 busy） */
  busy: boolean
  /** 页面内任一操作在途时禁用全部按钮（列表页任一行动作中） */
  busyAny?: boolean
  /** 在途操作类型：仅该按钮转圈（null 表示无在途操作） */
  pendingAction?: PendingAction | null
  /** 是否展示"调整规格"（仅详情页，external 不展示） */
  showResize?: boolean
  /** 按钮变体：列表页 ghost、详情页 soft */
  variant?: 'ghost' | 'soft' | 'outline'
  /** 按钮尺寸（列表页 sm，详情页默认） */
  size?: 'xs' | 'sm' | 'md'
  /** 是否在左侧展示状态徽章（详情页操作区）；在途时展示「启动中/关闭中/重启中」过渡徽章 */
  showBadge?: boolean
}>(), {
  busyAny: false,
  pendingAction: null,
  showResize: false,
  variant: 'ghost',
  size: 'md',
  showBadge: false
})

const emit = defineEmits<{
  start: []
  stop: []
  restart: []
  resize: []
  destroy: []
}>()

// 本组在途或页面内任一操作在途：全部禁用
const disabled = computed(() => props.busy || props.busyAny)

// 状态徽章：在途操作期间展示过渡徽章（启动中/关闭中/重启中），否则按实际状态
const badge = computed(() => props.pendingAction ? vmPendingStatusBadge(props.pendingAction) : vmStatusBadge(props.status))
</script>

<template>
  <div class="flex flex-wrap items-center gap-2">
    <UBadge
      v-if="showBadge"
      :color="badge.color"
      variant="soft"
      :label="badge.label"
    />
    <div
      v-if="showBadge"
      class="flex-1"
    />
    <UButton
      :size="size"
      :variant="variant"
      icon="i-lucide-play"
      :loading="busy && pendingAction === 'start'"
      :disabled="disabled || !canStartVM(status)"
      @click="emit('start')"
    >
      启动
    </UButton>
    <UButton
      :size="size"
      :variant="variant"
      icon="i-lucide-square"
      :loading="busy && pendingAction === 'stop'"
      :disabled="disabled || !canStopVM(status)"
      @click="emit('stop')"
    >
      关闭
    </UButton>
    <UButton
      :size="size"
      :variant="variant"
      icon="i-lucide-rotate-cw"
      :loading="busy && pendingAction === 'restart'"
      :disabled="disabled || !canRestartVM(status)"
      @click="emit('restart')"
    >
      重启
    </UButton>
    <UButton
      v-if="showResize && canResizeVM(status)"
      :size="size"
      :variant="variant"
      icon="i-lucide-arrows-up-down"
      :disabled="disabled"
      @click="emit('resize')"
    >
      调整规格
    </UButton>
    <UButton
      :size="size"
      :variant="variant"
      icon="i-lucide-trash-2"
      color="error"
      :disabled="disabled || !canDestroyVM(status)"
      @click="emit('destroy')"
    >
      销毁
    </UButton>
  </div>
</template>
