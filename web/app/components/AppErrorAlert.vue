<script setup lang="ts">
// 统一错误展示组件：后端契约错误体为 {"error": {"code", "message"}}
// code 为机器可读错误码，以徽章展示便于排障；message 为后端脱敏后的可读信息，直接展示
const props = defineProps<{
  /** 后端错误码（x-ms-error-code 头 / ErrorDetail.code 枚举） */
  code?: string
  /** 后端脱敏后的错误描述 */
  message?: string
  /** 展示标题，默认"操作失败" */
  title?: string
}>()

// 无错误信息时不渲染
const hasError = computed(() => Boolean(props.code || props.message))
</script>

<template>
  <UAlert
    v-if="hasError"
    color="error"
    variant="subtle"
    :title="title ?? '操作失败'"
    :description="message || '未知错误，请稍后重试'"
    icon="i-lucide-circle-alert"
  >
    <template
      v-if="code"
      #actions
    >
      <UBadge
        :label="code"
        color="error"
        variant="subtle"
        size="sm"
      />
    </template>
  </UAlert>
</template>
