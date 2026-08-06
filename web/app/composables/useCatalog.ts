/**
 * 目录映射（Zone/节点 id → 名称）：VM 列表页与详情页共用。
 * 模块级缓存：SPA 单实例，两页共享同一份数据，避免重复请求。
 * 加载失败静默吞掉（不阻塞 VM 列表），映射缺失时页面回退展示 id。
 * 缓存带失效机制：VM 增删改或 Zone/节点变更后，页面进入时调用 refresh() 重拉，
 * 避免名称映射长期过期；刷新期间保留旧映射（不闪回 id）。
 */
import { listZones } from '~/api/zones'

// 模块级状态：跨页面共享的懒加载缓存
const zoneNames = ref(new Map<number, string>())
const nodeNames = ref(new Map<number, string>())
const loaded = ref(false)
const loading = ref(false)
// 失效标记：invalidate() 置位；加载在途时置位则结束后自动补拉一次（补偿刷新）
let refreshPending = false

export function useCatalog() {
  /** 拉取目录（limit 取契约上限 100；超出上限时页面回退展示 id 并注明） */
  async function ensureLoaded(): Promise<void> {
    if (loaded.value && !refreshPending) return
    if (loading.value) {
      // 已有加载在途：标记补偿刷新，由在途请求完成后补拉
      refreshPending = true
      return
    }
    loading.value = true
    refreshPending = false
    try {
      const res = await listZones({ limit: 100 })
      const nextZones = new Map<number, string>()
      const nextNodes = new Map<number, string>()
      for (const zone of res.data) {
        nextZones.set(zone.id, zone.name)
        for (const node of zone.nodes) nextNodes.set(node.id, node.name)
      }
      zoneNames.value = nextZones
      nodeNames.value = nextNodes
      loaded.value = true
    } catch {
      // 目录加载失败不阻塞 VM 列表：保留旧映射，页面回退展示 id（页面兜底）
    } finally {
      loading.value = false
      if (refreshPending) {
        // 加载期间收到失效请求：补拉一次保证名称映射不过期
        refreshPending = false
        // 在途请求成功已把 loaded 置回 true；不复位则重入 ensureLoaded 命中短路直接返回，
        // 失效意图会被在途请求的成功结果吞掉（补偿刷新从未发生）。复位后强制补拉一次。
        loaded.value = false
        void ensureLoaded()
      }
    }
  }

  /** 目录失效：下次 ensureLoaded/refresh 时重新拉取（刷新期间保留旧映射） */
  function invalidate(): void {
    loaded.value = false
    refreshPending = true
  }

  /** 失效后立即重新拉取（VM 页进入时调用，保证名称映射不过期太久） */
  async function refresh(): Promise<void> {
    invalidate()
    await ensureLoaded()
  }

  /** Zone id → 名称（映射未就绪返回 undefined） */
  function zoneName(id: number): string | undefined {
    return zoneNames.value.get(id)
  }

  /** 节点 id → 名称（映射未就绪返回 undefined） */
  function nodeName(id: number): string | undefined {
    return nodeNames.value.get(id)
  }

  return { ensureLoaded, refresh, invalidate, zoneName, nodeName }
}
