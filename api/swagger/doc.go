// Package swagger 挂载 API 契约的在线浏览与输出处理器：
//   - GET /docs 渲染 Swagger UI 页面（纯 Go embed 静态资源，来自
//     github.com/swaggest/swgui）；
//   - GET /openapi.yaml 输出 OpenAPI 契约内容（application/yaml）。
//
// 契约副本同步说明：本包内嵌的 openapi.yaml 是 docs/openapi.yaml（契约
// 源）的字节级副本，go:embed 无法跨包读取 docs/ 下的文件，因此用副本。
// 修改契约后必须同步复制（保持字节一致，便于校验）：
//
//	cp docs/openapi.yaml api/swagger/openapi.yaml
//
// /docs 与 /openapi.yaml 两条路由刻意不写入契约本身（docs/openapi.yaml
// 的 paths），避免契约自指。
package swagger
