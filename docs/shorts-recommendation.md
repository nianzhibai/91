# 短视频推荐优化方案

## 1. 现状与问题

改造前 `GET /api/shorts/next?mode=recommend` 的行为是：取出全部可见视频 ID，
`rand.Shuffle` 打乱，把整个有序数组存进 `shorts_feed_sessions` 当快照，之后只按
`cursor` 推进。

也就是说「推荐」栏目等价于把整个库随机播放一遍，没有任何个性化。根本原因不在
排序算法，而在数据：

| 信号 | 现状 | 能否归因到人 |
| --- | --- | --- |
| `videos.views` | 只在详情页 `POST /api/video/{id}/view` 累加 | 否，全局计数器 |
| `videos.likes` | `handleLike` 只做 `likes + 1` | 否，全局计数器 |
| `video_reaction_visits` | 按每次页面实例的匿名 `visit_id` 记录 | 否，按设计不关联账号 |
| `video_favorites` | `(user_id, video_id, created_at)` | 是 |
| 观看时长 / 完播 / 划走 | 完全没有记录 | — |

所以系统唯一能用的个性化依据只有「收藏过什么」，而短视频页本身不产生任何反馈
数据：用户在短视频里划五百条，后端一条都没记住。

三个结论：

1. 推荐链路没有信号输入，`recommend` 分支拿不到 `viewerUserID` 的任何用处。
2. 短视频消费行为完全没被记录，任何排序算法的输入都是空的。
3. 点赞是全局计数器，无法回答「这个人喜欢什么」，且前端点赞态只存在内存里，
   刷新即丢。

## 2. 目标

在**不引入机器学习依赖**的前提下，让「推荐」栏目具备：

- 记住用户看过什么、看完没有、看了多久；
- 依据「刚看完的那条视频所在目录」做即时扩展（用户核心诉求）；
- 同目录推荐有节制，不把推荐流退化成目录连播；
- 目录信号不可靠时自动降级到标签信号；
- 全程可解释、可单测。

## 3. 数据模型

### 3.1 `video_likes`

账号级点赞。`videos.likes` 保留为冗余计数（与 `video_favorites` 同样的模式），
真实状态以本表为准。这样点赞可归因，且取消点赞是幂等的。

```sql
CREATE TABLE IF NOT EXISTS video_likes (
    user_id    INTEGER NOT NULL,
    video_id   TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, video_id)
);
```

### 3.2 `video_user_events`

一条记录 = 一次「这条视频被看了多久」的结算。`kind` 是对这次观看的分类，三个
取值互斥：

- `complete`：看完（含循环播放）
- `partial`：看了一部分
- `skip`：几乎没看

```sql
CREATE TABLE IF NOT EXISTS video_user_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL,
    video_id   TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('complete', 'partial', 'skip')),
    watch_ms   INTEGER NOT NULL DEFAULT 0,
    loop_count INTEGER NOT NULL DEFAULT 0,
    position   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
```

`position` 是这次观看在 feed 里的位置，只用于后续做位置偏差分析，不参与打分。

## 4. 算法

实现落在新包 `backend/internal/feed`，全部是纯函数，不碰数据库、不碰 HTTP。

### 4.1 目录画像

「同目录」是本站最强的信号（用户按目录整理视频 = 人工确认过的集合），但**不是
所有目录都是语义目录**。`下载于 2024-03` 这种归档目录里，同目录等于同批次，是噪声。

判据用目录内标签集中度：

```
share = 目录内最高频标签的出现次数 / 该目录的标签总数
```

- `share >= 0.4` → 语义目录，同目录召回生效
- `share < 0.4` → 归档目录，同目录召回关闭，退化成标签召回
- 目录完全没有标签 → 无证据，默认信任用户的目录结构（`Semantic = true`）

同时识别系列目录：目录内文件名去掉数字后同前缀的视频 ≥ 3 个即视为系列（`第3集`、
`第4集`…）。系列目录走自然序相邻推荐，非系列目录走随机同目录。

### 4.2 一批的配额

`recommend` 改成**按批决策**，不再预生成整轮快照。一批 5 条（前端 `BATCH_SIZE`）的
配额：

| 来源 | 比例 | 说明 |
| --- | --- | --- |
| 同目录 | 40% | 上一条刚消费的视频所在目录；系列目录按自然序取相邻集 |
| 同标签异目录 | 20% | 按打分取，排除当前目录 |
| 探索 | 20% | 均匀随机，独立配额不可省略 |
| 打分补齐 | 剩余 | 按 `score` 降序 |

**单条视频最多买 2 条同目录**（`count = 5` 时）。想继续看用户会自己划，不由算法
替用户决定 —— 这是防止「推荐」退化成「播放列表」的关键约束。

### 4.3 打分

```
score = 0.45 * tagAffinity
      + 0.25 * dirAffinity
      + 0.15 * freshness
      + 0.15 * quality
```

- `tagAffinity`：用户历史行为命中的标签权重之和，`1 - exp(-sum)` 压缩到 `[0,1]`
- `dirAffinity`：目录级完成率，贝叶斯平滑，见下
- `freshness`：`exp(-ageDays / 14)`
- `quality`：全库点赞的相对值，`(likes + 0.5) / (maxLikes + 1)`。稀疏库里接近常数，
  不构成噪声

所有计数都必须平滑，否则「1 次完播 = 100% 完播率」会永久霸榜：

```
dirAffinity = (completes + 5 * prior) / (impressions + 5)
```

### 4.4 排除

一批里排除两类视频：

- 本次会话已下发（`shorts_feed_sessions` 里保存的 served 列表）
- 最近 7 天已经消费过的（`video_user_events`）

## 5. 接口变更

### 5.1 `GET /api/shorts/next`

新增可选参数 `after=<videoID>`：客户端告诉服务端「用户刚看完这条」。服务端据此
决定这一批的同目录扩展。`latest` / `hot` / `favorites` 三个确定性栏目忽略它。

`recommend` 的响应语义变化：

- `total` 仍然是当前可见视频总数（保持与既有测试和前端校验一致）
- `nextCursor` 是本会话累计下发的条数，不再对应快照位置
- `roundComplete` 恒为 `false`；只有当候选池被排除干净（这一批取不到任何视频）时
  返回 `true` + 空列表，让前端换一轮

### 5.2 `POST /api/shorts/events`

批量上报观看结算，请求体：

```json
{ "events": [ { "videoId": "...", "kind": "complete", "watchMs": 12000, "loopCount": 1, "position": 3 } ] }
```

单次最多 32 条，超出截断。前端用 `keepalive` 的 `fetch` 异步发送，不阻塞滑动。

### 5.3 `POST /api/video/{id}/like` / `DELETE`

改为账号级幂等操作：写 `video_likes` 并同步 `videos.likes` 计数。`ShortsItemDTO`
新增 `liked` 字段，刷新后点赞态不再丢失。

## 6. 幂等与续播

`recommend` 模式下，服务端会话保存 `cursor -> 已下发 id 列表` 的记忆（仅内存），
同一个 `cursor` 的重试直接返回同一批，保证前端 `mergeShortsQueue` 的去重键
（`feedToken:feedCursor`）不会因为重算而错位。已下发 id 按序持久化到
`shorts_feed_sessions.video_ids`，进程重启后仍能避免重复推荐。

## 7. 前端

- 新增 `src/shorts/shortsTelemetry.ts`：观看时长累加器 + 上报队列。纯逻辑可单测。
- `ShortsSlideImpl` 在当前屏挂一个遥测 effect，切走或卸载时结算一条事件。
- 只统计真实播放时间：`timeupdate` 的媒体时间增量，`paused` 和 `document.hidden`
  期间不计入。
- 循环播放单独计数，且是最强的正向信号。

## 8. 验证

- `backend/internal/feed` 纯函数单测：配额、排除、系列相邻、语义目录开关、打分单调性
- `backend/internal/catalog` 单测：事件写入、画像聚合、点赞幂等
- `backend/internal/api` 单测：事件接口参数校验、`recommend` 批次决策与幂等重放
- `tests/shortsTelemetry.test.ts`：观看时长累加、循环检测、事件分类、队列刷写

线上观察（对单用户也成立的确定性指标）：

- **目录内连续观看条数**：上升说明同目录生效；涨得过猛说明配额给多了
- **目录内跳过率**：高于全局说明命中了归档目录，集中度判据没生效
- **一轮里的目录覆盖数**：太低说明多样性塌了

## 9. 不在本次范围

- `hot` 栏目仍按 `likes DESC` 排序，未改成时间衰减热度
- 详情页 `pickRelatedVideos` 未复用同一套打分（候选池仍是「该标签下最新 N 条」）
- 没有引入向量召回、多目标模型：库规模和样本量都不支持，收益远低于运维成本
