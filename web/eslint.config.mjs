// @ts-check
import withNuxt from './.nuxt/eslint.config.mjs'

export default withNuxt(
  {
    // 生成物是契约镜像（openapi-typescript 输出），禁止手改，不参与 lint 格式化校验
    ignores: ['app/api/generated/**']
  }
)
