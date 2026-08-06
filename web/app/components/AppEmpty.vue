<script setup lang="ts">
// 统一空态展示组件：列表无数据时复用
withDefaults(defineProps<{
  /** 空态标题，默认"暂无数据" */
  title?: string
  /** 空态描述 */
  description?: string
  /** 空态图标（Iconify 格式） */
  icon?: string
}>(), {
  title: '暂无数据',
  description: undefined,
  icon: 'i-lucide-inbox'
})
</script>

<template>
  <UEmpty
    :title="title"
    :description="description"
    :icon="icon"
  />
</template>
