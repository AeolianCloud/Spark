<script setup lang="ts">
import type { NavigationMenuItem } from '@nuxt/ui'

// 侧边导航项：全部管理模块入口（分组导航，便于后续扩展子项）
const items: NavigationMenuItem[][] = [[{
  label: 'Dashboard',
  icon: 'i-lucide-layout-dashboard',
  to: '/dashboard'
}, {
  label: '可用区',
  icon: 'i-lucide-boxes',
  to: '/zones'
}, {
  label: '节点',
  icon: 'i-lucide-server',
  to: '/nodes'
}, {
  label: 'IP 池',
  icon: 'i-lucide-network',
  to: '/ip-pools'
}, {
  label: '存储类型',
  icon: 'i-lucide-hard-drive',
  to: '/storage-types'
}, {
  label: '镜像',
  icon: 'i-lucide-image',
  to: '/images'
}, {
  label: '虚拟机',
  icon: 'i-lucide-monitor',
  to: '/vms'
}]]
</script>

<template>
  <UDashboardGroup storage-key="spark-web">
    <UDashboardSidebar collapsible>
      <template #header="{ collapsed }">
        <NuxtLink
          to="/dashboard"
          class="flex items-center gap-2 px-2"
        >
          <span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-primary text-primary-foreground">
            <UIcon
              name="i-lucide-zap"
              class="h-4 w-4"
            />
          </span>
          <span
            v-if="!collapsed"
            class="text-sm font-semibold truncate"
          >Spark 管理控制台</span>
        </NuxtLink>
      </template>

      <template #default="{ collapsed }">
        <UNavigationMenu
          :collapsed="collapsed"
          :items="items"
          orientation="vertical"
        />
      </template>

      <template #footer="{ collapsed }">
        <div class="flex w-full items-center justify-between gap-2">
          <span
            v-if="!collapsed"
            class="text-xs text-muted"
          >无鉴权管理面 · 请网络层隔离</span>
          <UColorModeButton />
        </div>
      </template>
    </UDashboardSidebar>

    <slot />
  </UDashboardGroup>
</template>
