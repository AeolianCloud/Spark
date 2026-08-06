<script setup lang="ts">
// 全局 SEO 与描述信息
useSeoMeta({
  title: 'Spark 管理控制台',
  description: 'Spark PVE 虚拟化平台管理界面',
  ogTitle: 'Spark 管理控制台',
  ogDescription: 'Spark PVE 虚拟化平台管理界面'
})
</script>

<template>
  <UApp>
    <NuxtLayout>
      <NuxtPage />
    </NuxtLayout>
  </UApp>
</template>
