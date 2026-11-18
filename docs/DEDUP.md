# 去重体系

去重分布在扫描入库、爬虫导入、前台软过滤和全库维护四个时机。哈希或采样指纹用于匹配字节证据，标题、封面与 teaser 内容帧用于匹配转码副本。扫描和软过滤还使用 `file_name + size_bytes` 弱兜底，存在同名同大小碰撞风险。

以下路径均相对 `backend/`：`internal/scanner/`（扫描）、`internal/drives/scriptcrawler/neardupe.go`（爬虫）、`internal/dedupe/`（规划）、`internal/catalog/`（软过滤、墓碑与事务）、`cmd/server/video_maintenance.go` 和 `video_dedupe_plan.go`（维护执行）、`internal/mediasim/`（相似度与阈值）。

```mermaid
flowchart LR
    A["① 扫描入库<br/>scanner"] --> C["③ 前台展示<br/>实时软过滤"]
    B["② 爬虫导入<br/>scriptcrawler"] --> C
    C --> D["④ 全库维护<br/>硬去重"]
```

## 判定信号

| 信号 | 是什么 | 能抓住什么 | 抓不住什么 |
|------|--------|-----------|-----------|
| `(drive_id, file_id)` | 视频恒等 ID | 同盘同文件的重复扫描（upsert 同一行） | 一切副本 |
| `content_hash` | 网盘原生哈希（如 115 SHA1、PikPak GCID） | 原生哈希值相同的文件，可跨盘匹配 | 不同算法产生不同哈希值的副本、转码副本 |
| `sampled_sha256` | `size + 采样字节` 的统一指纹，fingerprint worker 后台计算 | **跨盘**字节采样一致的文件 | 转码/水印副本 |
| `file_name + size_bytes` | 原生哈希缺失或未命中时的弱兜底 | 常见的同名同大小副本 | 改名副本；也可能碰撞不同内容 |
| 标题相似度 + 封面 SSIM | 归一化 Levenshtein（含核心段变体）+ 96×96 亮度 SSIM | 同源重发（标题几乎一样） | 改名重发、跨源转码 |
| teaser 帧签名 | teaser 按 1fps 抽 12 帧 96×96 灰度 | **跨源不同压制**（标题、封面全对不上也能抓） | 时长差 >2 秒的剪辑版 |

预览的**选段起点只由时长决定**，分档策略见 [README「异步生成」](../backend/README.md#异步生成)。时长接近的视频在 teaser 相同偏移处通常对应同一源画面，因此可比较不同压制、水印或裁切版本。

内容级判定规则，阈值常量统一在 `mediasim`：

| 规则 | 条件 | 结论 |
|------|------|------|
| 对齐 | 对齐帧 SSIM 中位数 ≥0.80 | 重复，自动处理 |
| 交叉兜底 | 对齐未命中、**时长精确相等**时：双方各至少 8 个有效帧，双向逐帧最优匹配，单帧 ≥0.95、双向 ≥75% 帧强匹配 | 重复（teaser 某段回退到备选起点造成整段错位的场景） |

亮度标准差 <6 的纯色/黑场帧不参与统计；有效比较帧少于 6 不判重；时长不足 120 秒不启用内容通道，避免短视频时长碰撞。

## 四个时机

**① 扫描入库（`internal/scanner`）**——同 `(drive_id, file_id)` 重复扫描只 upsert 同一行；`deleted_videos` 墓碑按 id / file_id / content_hash / 文件名+大小 拦截已删除视频回流；墓碑只拦截新入库，库内仍绑定该网盘文件的视频照常更新。原生哈希相同，或采样指纹和大小均相同，即命中精确规则。文件名和大小用于弱兜底。文件大小或已知哈希变化时，扫描会清除旧指纹、封面和预览并安排重新生成。无法用哈希比较时，扫描参考源文件的修改时间：第一次取得的修改时间只作为基准记录；之后修改时间变化，扫描先保留封面和预览，把旧采样指纹留作对照并重新计算。新指纹相同则只更新修改时间；不同则清除封面、预览和原生哈希并重新生成。没有旧指纹可对照的记录直接重新生成。核对完成前，该视频不参与标题/封面和内容通道的判重。

**② 爬虫导入（`internal/drives/scriptcrawler`）**——层层拦截，重复视频在**上传网盘之前**就被挡下：

```mermaid
flowchart TD
    A["脚本产出新源视频"] --> B{"crawler_seen_sources<br/>已处理过？"}
    B -- 是 --> SKIP1["跳过，不下载"]
    B -- 否 --> C["下载到本地 + 校验<br/>计算 sampled_sha256"]
    C --> D{"库内已有相同指纹？"}
    D -- 是 --> SKIP2["删本地文件<br/>标记 duplicate 已见"]
    D -- 否 --> E["取时长 ±2s 候选（≤200）"]
    E --> F{"标题 ≥0.90 且<br/>封面 SSIM ≥0.95？"}
    F -- 命中 --> SKIP2
    F -- 未命中 --> G{"内容通道（≥120s）：<br/>按候选 teaser 选段时间戳<br/>从本地新视频抽帧比对"}
    G -- 命中 --> SKIP2
    G -- 未命中 --> J["正常入库 + 挂标签<br/>标记 imported"]
```

标记过 `crawler_seen_sources` 的源永远不会再下载，即使对应视频后来被夜间维护删掉——这保证"删重复"不会引起"再爬回来"的循环。

爬虫始终保留已入库的视频，即使新下载的副本更大。命中后，Catalog 在同一个事务中记录判重依据和爬虫已见状态。

**③ 前台展示（实时软过滤）**——`content_hash`、`size_bytes + sampled_sha256` 或弱兜底 `file_name + size_bytes` 相同的视频，前台列表和封面/预览生成队列在可见且有源文件的记录中选择最早入库的那条（`uniqueVideoWhereSQL`）。行还在库里，作用是在夜间硬去重跑到之前不给用户看重复、不给副本白白生成资产。

**④ 全库维护（`internal/dedupe` + `cmd/server/video_maintenance.go`）**——定时维护和后台「扫描所有网盘」最终都会执行。Planner 只读计算完整计划，三个通道串行，前一通道的逻辑删除项不进下一通道：

```mermaid
flowchart TD
    S["可见且有源文件的视频"] --> E1["① 精确分组并选 canonical<br/>content_hash 或 size + sampled_sha256"]
    E1 --> E2["② 剩余项按标题/封面分组并选 canonical<br/>时长 ±2s + 前缀桶/q-gram 预过滤<br/>标题 ≥0.90 且封面 SSIM ≥0.95"]
    E2 --> E3["③ 剩余项按内容分组并选 canonical<br/>≥120s、时长 ±2s 滑动窗口<br/>帧签名（framesigs/ 磁盘缓存）"]
    E3 --> R1{"对齐中位数 ≥0.80？"}
    R1 -- 是 --> DUP["并查集成组"]
    R1 -- 否 --> CR{"时长精确相等<br/>且交叉匹配命中？"}
    CR -- 是 --> DUP
    CR -- 否 --> PASS["不是重复"]
    DUP --> CANON["并查集连通组内选保留项"]
    CANON --> FINAL["解析跨通道 canonical 链<br/>全部指向最终存活项"]
    FINAL --> TX["一次 SQLite 事务<br/>合并用户状态/引用 + 旧 ID 映射 + 退役重复行 + 清理任务"]
    TX --> DEL["提交后幂等清理本地资产<br/>teaser/封面及派生图/帧签名"]
```

每个通道都从该通道形成的重复组内部选择 canonical，而不是从全库另找视频。精确通道按 **本地资产完整度 > 入库早**；标题/封面与内容通道按 **体积大 > 资产完整度 > 入库早**——优先留原版画质。重复关系按传递闭包成组：A 命中 B、B 命中 C 时，A/B/C 属于同一个重复组。

Planner 跑完全部通道后会压缩 canonical 链。例如精确通道先得到 `B → A`，内容通道又得到 `A → C`，最终事务写成 `B → C`、`A → C`，不会留下指向中间删除项的引用。

提交前会校验删除项和最终 canonical 的视频修订；规划期间若来源、匹配信号或资产版本发生变化，整个事务回滚并基于最新数据重新规划（最多三次），观看和点赞计数变化不影响计划。

合并语义（所有通道一致）：先把重复项的标签、访问反应、观看/点赞等计数、分享和历史任务引用合并到最终保留项，再把旧公开 ID、保留项 ID 和来源快照写入 `merged_videos`，退役重复行。旧链接解析到当前保留项，连续合并会更新已有映射。原网盘文件保留，扫描根据来源快照和保留项判断是否继续跳过。文件被替换后重新走入库判定，原 ID 仍属于原视频。自动合并只增加已合并记录；黑名单列表和清理任务只接收用户主动屏蔽的来源。事务同时登记 `duplicate_asset_cleanup_jobs`，提交后幂等清理本地 teaser、普通封面、Shorts 背景封面和帧签名缓存；进程中断或文件系统临时失败时由下一轮维护重试。

## 已合并视频管理 API

管理员调用 `GET /admin/api/merged-videos` 分页查询（`page`、`size`，可按 `canonicalVideoId`、`driveId`、`sourceState` 筛选）。`GET /admin/api/merged-videos/{id}` 按副本原 ID 返回来源快照、`sourceRevision` 及当前是否有活动视频共用源文件。来源状态 `recorded` 表示已记录，`gone` 表示确认消失，`replaced` 表示该位置的文件已经改变。

`DELETE /admin/api/merged-videos/{id}/source` 接收 `{"sourceRevision":"…"}`。核对来源后删除物理文件，保留旧 ID 映射；同一版本重试返回成功。缺少版本返回 400，未知 ID 返回 404。版本过期、共享源文件、文件被替换、身份无法核实或另一次删除正在执行，返回 409 和具体错误码。管理统计 `GET /admin/api/videos/stats` 的 `merged` 按已合并的原 ID 计数。

## 用户删除

前台“不再展示”和后台“仅删除记录”会把整组视频加入黑名单，保留网盘文件。分组沿共享源文件和库内已有的判重关系查找，涵盖跨盘副本、维护已合并的副本，以及扫描时记下并跳过的文件。

后台勾选删除源文件时，先调用 `GET /admin/api/videos/{id}/deletion-preview`。成功返回 `200`，包含独立源文件列表 `sources`、版本 `groupRevision` 和 `requiresConfirmation`。列表首项是选中视频的文件。多条记录共用一个文件时只列一项，可携带预览版本直接提交删除。

如果 `requiresConfirmation` 为 `true`，前端询问是否一并删除重复文件。用户确认后调用 `DELETE /admin/api/videos/{id}`，提交 `deleteSource: true`、预览的 `groupRevision` 和 `includeDuplicates`。选择 `true` 删除全部文件，选择 `false` 只删除当前文件及其引用记录。用户取消即可关闭弹窗。删除成功返回 `200`。预览后范围变化时返回 `409 deletion_scope_changed`，前端重新获取预览。

跨盘删除逐个完成，用户选中的文件最后处理。某个网盘失败时可以从原视频重新发起请求。扫描历史中的源文件在物理删除前会向网盘核对当前信息。

## 缓存与工具

- 帧签名按 `(teaser size, mtime)` 缓存在 `previews/framesigs/`，预览更新后失效，视频删除时清理；全库维护和爬虫内容通道共用缓存。
- 在 `backend/` 执行 `go run ./cmd/dedupe-dryrun` 默认仅预演内容通道，不合并视频或删除资产；工具打开数据库时仍可能执行 schema 初始化或迁移。通过 `-db` / `-local-dir` 指定数据库与预览目录；加 `-apply` 提交计划，沿用维护的合并事务和资产清理路径。
- 重复决策的 SQL 查询、证据和备份规则见 [内部记录](../backend/internal/dedupe/RECORDS.md)。
