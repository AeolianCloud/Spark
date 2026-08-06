<script setup lang="ts">
// 根路径客户端重定向到 Dashboard。
// dev 场景 nitro routeRules 已处理重定向；静态托管（generate）下该运行时重定向不生效，
// 此处提供客户端路由层重定向兜底，保证 SPA 下访问 / 也能进入 Dashboard
definePageMeta({ redirect: '/dashboard' })
</script>
