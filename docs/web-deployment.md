# Web 管理界面部署

本文档说明 Spark Web 管理界面（`web/`）的生产部署方式：**静态产物 + nginx 托管**，`/api` 反向代理到后端进程。后端部署（数据库、PVE 节点准备、配置项）见仓库根 [README](../README.md)。

## 架构

```
浏览器 ──> nginx(:80/:443)
              ├── /       → web/ 静态产物（SPA）
              └── /api/*  → 反向代理，剥离 /api 前缀后转发后端进程（默认 127.0.0.1:8080）
```

- 前端为 **Nuxt SPA**（SSR 关闭），构建产物为纯静态文件，由 nginx 直接托管，无 Node 运行时。
- 后端进程（`./server`，默认监听 `:8080`）**没有 `/api` 前缀路由**：所有路由均为根级（`/vms`、`/zones` 等）。因此 nginx 必须**剥离前缀**——`proxy_pass http://127.0.0.1:8080/;` 带尾部斜杠即完成替换（`/api/vms` → `/vms`）。若配置为不带尾斜杠的 `proxy_pass http://127.0.0.1:8080;`，则 `URI` 原样透传为 `/api/vms`，后端将 404。
- SPA 历史路由（如 `/vms/42`）需回退到 `index.html`，由前端路由接管渲染。
- 后端另有 Swagger UI（`/docs`）与契约原文（`/openapi.yaml`）两条路由，如需对外提供可直接在 nginx 同样反代，本部署不强制。

## 构建

前置：Node.js >= 22.19.0，npm（版本锁定见 `web/package.json` 的 `engines` 与 `packageManager`）。

```bash
cd web
npm ci                # 按 lockfile 安装依赖（postinstall 自动执行 nuxt prepare）
npm run api:check     # 契约一致性校验（CI 也会跑，本地构建前跑一次确保 client 与 docs/openapi.yaml 对齐）
npm run generate      # 静态站点生成
```

产物位于 `web/.output/public/`，包含 `index.html`、各路由 SPA 壳及 `404.html`。

**为何用 `generate` 而非 `build`**：本项目 SSR 关闭（SPA 模式），`nuxt build` 的产物只含 `_nuxt` 静态资源与 server 目录，**没有 `index.html`**，无法被 nginx 静态托管；`nuxt generate` 才会产出完整的静态站点（含 SPA 入口壳）。详见 `web/README.md` 的「常用命令」一节。

将 `.output/public/` 内容（或整个目录）拷贝/挂载到 nginx 的静态根即可，例如：

```bash
scp -r .output/public/* /var/www/spark-web/
```

## nginx 配置示例

```nginx
server {
    listen 80;
    server_name spark.example.com;

    # 静态根：构建产物 .output/public 的部署位置
    root /var/www/spark-web;

    index index.html;

    # SPA 回退：未知路径与历史路由均落到 index.html
    location / {
        try_files $uri $uri/ /index.html;
    }

    # /api 反向代理到后端进程：尾部斜杠剥离 /api 前缀
    # /api/vms → http://127.0.0.1:8080/vms
    location /api/ {
        proxy_pass http://127.0.0.1:8080/;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # 可选：文本资源 gzip
    gzip on;
    gzip_types text/plain text/css application/javascript application/json image/svg+xml;
    gzip_min_length 1024;
}
```

配置自查要点：

- `location /api/` 的 `proxy_pass` **必须以 `/` 结尾**（`http://127.0.0.1:8080/`），这是前缀剥离的关键；漏掉尾斜杠会导致后端 404。
- `try_files $uri $uri/ /index.html` 是 SPA 必需，否则刷新 `/vms/42` 这类路径会直接 404。
- 若后端监听非 8080 端口，同步修改 `proxy_pass`（端口由 `server.port` / `SPARK_SERVER_PORT` 决定，见根 README 配置表）。
- 生产建议开启 TLS（`listen 443 ssl` + 证书），并补 `proxy_set_header X-Forwarded-Proto https`。

## 安全提示：第一批无鉴权（必须阅读）

> 设计决策 D4（openspec change `web-management-ui`）：**第一批不做登录/鉴权**，界面本身没有任何登录交互。

- 任何能访问该界面/IP 的人都拥有**全部 VM 操作权**（创建、启停、升降配、销毁）以及基础设施管理权（节点、IP 池、存储、镜像）。
- 部署方**必须**在网络层实施隔离，任选其一或组合：
  - 仅在内网/VPN 内提供服务（`listen` 绑定内网地址，不暴露公网）；
  - 防火墙只放行可信来源 IP；
  - nginx 增加 HTTP Basic Auth（`auth_basic`）等前置认证。
- **不要把部署端口（尤其公网）暴露到不受信网络**。
- 鉴权将在后续独立变更中引入（届时同步更新 `docs/openapi.yaml` 的 `security` 声明）。

## 相关文档

- 仓库根 [README](../README.md)：后端构建/配置/启动、PVE 节点准备、API 概览
- [API 契约](../docs/openapi.yaml)：OpenAPI 3.0 唯一事实来源（部署副本 `api/swagger/openapi.yaml` 与其字节一致）；前端 client 由它生成
- [API 错误码契约](api-errors.md)：错误码语义与稳定性约定
- [架构决策记录](adr/)：领域与架构决策（ADR 0003 契约、ADR 0004 敏感字段加密等）
- `web/README.md`：前端构建命令、API client 说明、CI 契约校验
