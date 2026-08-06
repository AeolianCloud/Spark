<script setup lang="ts">
// 统一加载态组件：骨架屏占位，rows 控制占位行数，供列表/详情页复用
withDefaults(defineProps<{
  /** 骨架占位行数 */
  rows?: number
}>(), {
  rows: 3
})
</script>

<template>
  <div class="space-y-4 p-4">
    <USkeleton
      v-for="i in rows"
      :key="i"
      class="h-10 w-full"
    />
  </div>
</template>
