<script setup lang="ts">
import type { NavigationMenuItem } from '@nuxt/ui'
import { useAuth } from '~/composables/useAuth'

// 登录态：页脚展示当前登录管理员身份，提供登出入口（登出走 useAuth.logout）
const { identity, logout } = useAuth()

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

      <template #footer>
        <div class="flex w-full items-center justify-between gap-2">
          <span
            class="min-w-0 truncate text-xs text-muted"
            :title="identity?.username"
          >{{ identity?.username }}</span>
          <div class="flex shrink-0 items-center gap-1">
            <UButton
              icon="i-lucide-log-out"
              variant="ghost"
              color="neutral"
              size="sm"
              aria-label="退出登录"
              @click="logout"
            />
            <UColorModeButton />
          </div>
        </div>
      </template>
    </UDashboardSidebar>

    <slot />
  </UDashboardGroup>
</template>
