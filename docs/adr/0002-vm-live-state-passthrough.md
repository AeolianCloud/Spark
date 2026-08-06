# VM 实时状态透传查询，不落库

VM 的实时运行状态（如电源状态、任务进度）不属于业务持久化数据，数据库 vms 表的状态相关字段只有 `pve_vmid` 与 `provision_error`，预配置状态（creating/failed/ready）由二者推导（pve_vmid 为 0 → creating，provision_error 非空 → failed，其余 → ready）；ready 仅作 create 响应占位，list/detail 对已供给 VM 一律透传 PVE 实时状态。实时状态在请求时向 PVE 查询，避免状态双写导致的缓存不一致与竞态（原设计 D1）。

## Consequences

- 查询 VM 列表时需要批量向 PVE 拉取状态，属可接受的扇出成本
- PVE 不可达时 VM 列表降级返回已存储的静态字段
