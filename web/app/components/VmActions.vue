<script setup lang="ts">
// 生命周期操作按钮组：VM 列表页与详情页共用，仅负责按状态渲染按钮；
// 实际请求、toast 与错误展示由父组件处理（两页的 busy/错误语义不同）。
import {
  canDestroyVM,
  canResizeVM,
  canRestartVM,
  canStartVM,
  canStopVM,
  vmStatusBadge
} from '~/utils/vm'

const props = withDefaults(defineProps<{
  /** VM 状态：决定哪些操作可用 */
  status: string
  /** 本组操作是否进行中（仅本按钮组 loading，如列表页按行 busy） */
  busy: boolean
  /** 页面内任一操作在途时禁用全部按钮（列表页任一行动作中） */
  busyAny?: boolean
  /** 是否展示"调整规格"（仅详情页） */
  showResize?: boolean
  /** 按钮变体：列表页 ghost、详情页 soft */
  variant?: 'ghost' | 'soft' | 'outline'
  /** 按钮尺寸（列表页 sm，详情页默认） */
  size?: 'xs' | 'sm' | 'md'
  /** 是否在左侧展示状态徽章（详情页操作区） */
  showBadge?: boolean
  /** 销毁按钮恒显示（不可用时禁用，列表页口径）；默认按状态显隐（详情页口径） */
  alwaysShowDestroy?: boolean
}>(), {
  busyAny: false,
  showResize: false,
  variant: 'ghost',
  size: 'md',
  showBadge: false,
  alwaysShowDestroy: false
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
</script>

<template>
  <div class="flex flex-wrap items-center gap-2">
    <UBadge
      v-if="showBadge"
      :color="vmStatusBadge(status).color"
      variant="soft"
      :label="vmStatusBadge(status).label"
    />
    <div
      v-if="showBadge"
      class="flex-1"
    />
    <UButton
      v-if="canStartVM(status)"
      :size="size"
      :variant="variant"
      icon="i-lucide-play"
      :loading="busy"
      :disabled="disabled"
      @click="emit('start')"
    >
      启动
    </UButton>
    <UButton
      v-if="canStopVM(status)"
      :size="size"
      :variant="variant"
      icon="i-lucide-square"
      :loading="busy"
      :disabled="disabled"
      @click="emit('stop')"
    >
      关闭
    </UButton>
    <UButton
      v-if="canRestartVM(status)"
      :size="size"
      :variant="variant"
      icon="i-lucide-rotate-cw"
      :loading="busy"
      :disabled="disabled"
      @click="emit('restart')"
    >
      重启
    </UButton>
    <UButton
      v-if="showResize && canResizeVM(status)"
      :size="size"
      variant="outline"
      icon="i-lucide-arrows-up-down"
      :disabled="disabled"
      @click="emit('resize')"
    >
      调整规格
    </UButton>
    <UButton
      v-if="alwaysShowDestroy || canDestroyVM(status)"
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
