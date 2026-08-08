<script setup lang="ts">
// 管理员登录页：独立轻量布局（不套默认侧边栏，layout: false），居中卡片表单。
// 提交调 useAuth.login()（内部走 POST /auth/admin/login，登录请求不注入令牌、
// 401 不触发全局跳转），成功跳转首页，失败展示后端脱敏错误信息并停留。
import { ApiError } from '~/api/client'
import { useAuth } from '~/composables/useAuth'
import type { FormValidateError } from '~/utils/format'

definePageMeta({ layout: false })

const { login } = useAuth()

const form = reactive({
  username: '',
  password: ''
})
const submitting = ref(false)
const loginError = ref<ApiError | null>(null)
// 表单实例引用：卡片 footer 提交按钮经由 form.submit() 触发校验与 @submit
const formRef = ref<{ submit: () => Promise<void> }>()

function validate(): FormValidateError[] {
  const errors: FormValidateError[] = []
  if (!form.username.trim()) {
    errors.push({ name: 'username', message: '请输入管理员账号' })
  }
  if (!form.password) {
    errors.push({ name: 'password', message: '请输入密码' })
  }
  return errors
}

async function onSubmit(): Promise<void> {
  if (submitting.value) return
  submitting.value = true
  loginError.value = null
  try {
    await login(form.username.trim(), form.password)
    await navigateTo('/dashboard')
  } catch (err) {
    // 登录失败：仅展示后端脱敏后的 ApiError message，不展示任何令牌/敏感信息；
    // 非 ApiError 的未知异常（如运行时 bug）也兜底为固定文案，保证任何失败都有可见反馈
    loginError.value = err instanceof ApiError
      ? err
      : new ApiError(0, 'unknown', '登录失败，请稍后重试')
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <main class="flex min-h-screen items-center justify-center bg-background p-4">
    <UCard class="w-full max-w-sm">
      <template #header>
        <div class="flex items-center gap-2">
          <span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-primary text-primary-foreground">
            <UIcon
              name="i-lucide-zap"
              class="h-4 w-4"
            />
          </span>
          <div class="min-w-0">
            <p class="text-sm font-semibold">
              Spark 管理控制台
            </p>
            <p class="text-xs text-muted">
              管理员登录
            </p>
          </div>
        </div>
      </template>

      <UForm
        ref="formRef"
        :validate="validate"
        :validate-on="['blur', 'change']"
        class="space-y-4"
        @submit="onSubmit"
      >
        <UFormField
          name="username"
          label="账号"
          required
        >
          <UInput
            v-model="form.username"
            placeholder="管理员账号"
            autocomplete="username"
          />
        </UFormField>

        <UFormField
          name="password"
          label="密码"
          required
        >
          <UInput
            v-model="form.password"
            type="password"
            placeholder="密码"
            autocomplete="current-password"
          />
        </UFormField>

        <AppErrorAlert
          v-if="loginError"
          :code="loginError.code"
          :message="loginError.message"
          title="登录失败"
        />
      </UForm>

      <template #footer>
        <UButton
          color="primary"
          class="w-full"
          icon="i-lucide-log-in"
          :loading="submitting"
          :disabled="submitting"
          @click="formRef?.submit()"
        >
          登录
        </UButton>
      </template>
    </UCard>
  </main>
</template>
