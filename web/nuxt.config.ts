// https://nuxt.com/docs/api/configuration/nuxt-config
export default defineNuxtConfig({
  modules: [
    '@nuxt/eslint',
    '@nuxt/ui'
  ],

  // SPA 模式：静态托管使用 `nuxt generate` 生成（产物 .output/public 含 index.html 与各路由 SPA 壳）；
  // `nuxt build` 产物仅含 _nuxt 资源，无 index.html，不适用于纯静态托管
  ssr: false,

  devtools: {
    enabled: true
  },

  app: {
    head: {
      htmlAttrs: {
        lang: 'zh-CN'
      },
      title: 'Spark 管理控制台',
      meta: [
        { name: 'viewport', content: 'width=device-width, initial-scale=1' },
        { name: 'description', content: 'Spark PVE 虚拟化平台管理界面' }
      ],
      link: [
        { rel: 'icon', href: '/favicon.ico' }
      ]
    }
  },

  css: ['~/assets/css/main.css'],

  // 根路径重定向到 Dashboard。
  // dev 场景由 nitro 的 routeRules 处理；静态托管场景（generate）下该运行时重定向不生效，
  // 根路径由 app/pages/index.vue 在客户端路由层重定向（SPA 下同样生效）
  routeRules: {
    '/': { redirect: '/dashboard' }
  },

  compatibilityDate: '2026-06-30',

  // 开发环境将 /api 代理到本地后端（默认 :8080），避免跨域；
  // 生产环境由 nginx 静态托管并反向代理 /api（见 design D8）
  nitro: {
    devProxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true
      }
    }
  },

  // TypeScript 严格模式
  typescript: {
    strict: true
  },

  eslint: {
    config: {
      stylistic: {
        commaDangle: 'never',
        braceStyle: '1tbs'
      }
    }
  }
})
