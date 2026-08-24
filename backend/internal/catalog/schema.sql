-- 视频元数据主表
CREATE TABLE IF NOT EXISTS videos (
    id               TEXT PRIMARY KEY,          -- <drive>-<fileID> 拼接的稳定 ID
    drive_id         TEXT NOT NULL,
    file_id          TEXT NOT NULL,
    file_name        TEXT DEFAULT '',           -- 网盘侧原始文件名，用于同名同大小去重
    content_hash     TEXT DEFAULT '',
    sampled_sha256   TEXT DEFAULT '',           -- 跨网盘统一采样指纹（size + sampled bytes）
    fingerprint_status TEXT DEFAULT 'pending',  -- pending / ready / failed
    fingerprint_error  TEXT DEFAULT '',
    parent_id        TEXT,
    dir_name         TEXT DEFAULT '',           -- 所在目录名（扫盘时落库，供标签重算使用）
    title            TEXT NOT NULL,
    author           TEXT,
    tags             TEXT,                      -- JSON array
    duration_seconds INTEGER DEFAULT 0,
    size_bytes       INTEGER DEFAULT 0,
    ext              TEXT,
    thumbnail_url    TEXT,
    thumbnail_updated_at INTEGER DEFAULT 0,     -- thumbnail-only revision; unrelated metadata must not invalidate image caches
    thumbnail_status TEXT DEFAULT 'pending',    -- pending / ready / failed / skipped
    thumbnail_failures INTEGER DEFAULT 0,        -- consecutive transient thumbnail generation failures
    preview_file_id  TEXT,                      -- deprecated: 旧版回写网盘后的预览视频 file id
    preview_local    TEXT,                      -- 本地预览视频路径（兜底）
    preview_updated_at INTEGER DEFAULT 0,       -- preview-only revision; unrelated metadata must not invalidate teaser caches
    preview_status   TEXT DEFAULT 'pending',    -- pending / ready / failed / disabled
    transcode_status TEXT DEFAULT '',           -- '' / pending / ready / skipped / failed（浏览器兼容性转码）
    transcode_error  TEXT DEFAULT '',
    transcoded_file_id TEXT DEFAULT '',         -- 转码产物在同一 drive 上的 fileID，播放源优先用它
    transcoded_size  INTEGER DEFAULT 0,
    views            INTEGER DEFAULT 0,
    last_viewed_at   INTEGER DEFAULT 0,
    favorites        INTEGER DEFAULT 0,
    comments         INTEGER DEFAULT 0,
    likes            INTEGER DEFAULT 0,
    last_liked_at    INTEGER DEFAULT 0,
    dislikes         INTEGER DEFAULT 0,
    hidden           INTEGER DEFAULT 0,          -- 1 = hidden from public display
    is_canonical     INTEGER NOT NULL DEFAULT 1, -- derived by dedup triggers; hidden rows still participate
    tags_manual      INTEGER DEFAULT 0,          -- 1 = user explicitly curated tags
    badges           TEXT,                      -- JSON array
    description      TEXT,
    published_at     INTEGER NOT NULL,          -- unix ms
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_videos_drive ON videos(drive_id, file_id);
CREATE INDEX IF NOT EXISTS idx_videos_pub   ON videos(published_at DESC);
CREATE INDEX IF NOT EXISTS idx_videos_created ON videos(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_videos_duration ON videos(duration_seconds);
CREATE INDEX IF NOT EXISTS idx_videos_views ON videos(views DESC);

-- Exact per-basis representatives used to project tags from duplicate source
-- rows without collapsing the three independent deduplication relations into
-- one lossy canonical_video_id.
CREATE TABLE IF NOT EXISTS video_dedup_representatives (
    video_id          TEXT NOT NULL,
    basis             TEXT NOT NULL CHECK (basis IN ('self', 'content_hash', 'sampled_sha256', 'file_name_size')),
    representative_id TEXT NOT NULL,
    PRIMARY KEY (video_id, basis)
);

CREATE INDEX IF NOT EXISTS idx_video_dedup_representative
    ON video_dedup_representatives(representative_id, video_id);

-- 管理员提交的视频直链后台任务。source_url 只在任务排队或执行期间保留；
-- 进入 completed / failed / canceled 后由状态更新语句立即清空。
CREATE TABLE IF NOT EXISTS remote_upload_jobs (
    sequence           INTEGER PRIMARY KEY AUTOINCREMENT,
    id                 TEXT NOT NULL UNIQUE,
    source_url         TEXT NOT NULL DEFAULT '',
    source_label       TEXT NOT NULL DEFAULT '',
    requested_title    TEXT NOT NULL DEFAULT '',
    resolved_title     TEXT NOT NULL DEFAULT '',
    tags               TEXT NOT NULL DEFAULT '[]',
    state              TEXT NOT NULL
                           CHECK (state IN (
                               'queued', 'downloading', 'validating', 'saving',
                               'completed', 'failed', 'canceled'
                           )),
    bytes_downloaded   INTEGER NOT NULL DEFAULT 0,
    total_bytes        INTEGER NOT NULL DEFAULT 0,
    cancel_requested   INTEGER NOT NULL DEFAULT 0,
    error_message      TEXT NOT NULL DEFAULT '',
    temp_file          TEXT NOT NULL DEFAULT '',
    final_file         TEXT NOT NULL DEFAULT '',
    completed_video_id TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    started_at         INTEGER NOT NULL DEFAULT 0,
    updated_at         INTEGER NOT NULL,
    finished_at        INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_remote_upload_jobs_queue
    ON remote_upload_jobs(state, sequence);
CREATE INDEX IF NOT EXISTS idx_remote_upload_jobs_recent
    ON remote_upload_jobs(sequence DESC);
CREATE INDEX IF NOT EXISTS idx_remote_upload_jobs_finished
    ON remote_upload_jobs(finished_at);

-- 视频详情页按“本次访问”记录的一张匿名临时选票。visit_id 由每次页面实例
-- 随机生成，不关联账号、Cookie 或设备；刷新/重新进入会生成新的 visit_id。
-- 保留 none 行可以让取消操作和重复请求保持幂等。
CREATE TABLE IF NOT EXISTS video_reaction_visits (
    video_id   TEXT NOT NULL,
    visit_id   TEXT NOT NULL,
    reaction   TEXT NOT NULL DEFAULT 'none'
                   CHECK (reaction IN ('none', 'like', 'dislike')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (video_id, visit_id)
);

CREATE INDEX IF NOT EXISTS idx_video_reaction_visits_video
    ON video_reaction_visits(video_id);

-- 统一标签池
CREATE TABLE IF NOT EXISTS tags (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    label       TEXT NOT NULL UNIQUE COLLATE NOCASE,
    aliases     TEXT NOT NULL DEFAULT '[]',       -- JSON array，旧版别名数据，保留用于迁移兼容
    -- 匹配规则 JSON：{"keywords":[],"matchAvCode":bool}
    -- 为空时匹配器按 label+旧版 aliases 兜底。
    match_rules TEXT NOT NULL DEFAULT '{}',
    source      TEXT NOT NULL DEFAULT 'user',     -- builtin / user / generated
    origin      TEXT NOT NULL DEFAULT '',         -- crawler 等来源型标签标记；不参与匹配来源归一
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS video_tags (
    video_id   TEXT NOT NULL,
    tag_id     INTEGER NOT NULL,
    -- auto=规则引擎 / manual=人工 / legacy=旧数据回填 / crawler=爬虫脚本或爬虫名 /
    -- series=番号系列 / propagated=同类传播
    source     TEXT NOT NULL DEFAULT 'auto',
    evidence   TEXT NOT NULL DEFAULT '',          -- 命中证据，如 "文件名:翘臀"
    created_at INTEGER NOT NULL,
    PRIMARY KEY (video_id, tag_id)
);

CREATE INDEX IF NOT EXISTS idx_video_tags_tag ON video_tags(tag_id);
CREATE INDEX IF NOT EXISTS idx_video_tags_video ON video_tags(video_id);

-- 被拉黑、删除或自动去重的视频。用于防止后续扫描 / 爬虫把同一个源文件
-- 再次入库；source_deleted 是旧版本兼容字段，源文件删除成功后会清除墓碑。
CREATE TABLE IF NOT EXISTS deleted_videos (
    id                 TEXT PRIMARY KEY,
    drive_id           TEXT NOT NULL DEFAULT '',
    file_id            TEXT NOT NULL DEFAULT '',
    parent_id          TEXT NOT NULL DEFAULT '',
    content_hash       TEXT NOT NULL DEFAULT '',
    file_name          TEXT NOT NULL DEFAULT '',
    size_bytes         INTEGER NOT NULL DEFAULT 0,
    reason             TEXT NOT NULL DEFAULT '',
    source_deleted     INTEGER NOT NULL DEFAULT 0,
    canonical_video_id TEXT NOT NULL DEFAULT '',
    restore_requested  INTEGER NOT NULL DEFAULT 0,
    restore_payload    TEXT NOT NULL DEFAULT '',
    deleted_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_deleted_videos_drive_file
    ON deleted_videos(drive_id, file_id);
CREATE INDEX IF NOT EXISTS idx_deleted_videos_drive_hash
    ON deleted_videos(drive_id, content_hash);
CREATE INDEX IF NOT EXISTS idx_deleted_videos_drive_signature
    ON deleted_videos(drive_id, file_name, size_bytes);

-- 爬虫来源记录。用于把已确认重复的 source_id 写回 seen 列表，
-- 避免后续爬虫反复下载同一个候选视频。
CREATE TABLE IF NOT EXISTS crawler_seen_sources (
    kind               TEXT NOT NULL,
    drive_id           TEXT NOT NULL,
    source_id          TEXT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'imported', -- imported / duplicate
    canonical_video_id TEXT NOT NULL DEFAULT '',
    sampled_sha256     TEXT NOT NULL DEFAULT '',
    size_bytes         INTEGER NOT NULL DEFAULT 0,
    first_seen_at      INTEGER NOT NULL,
    last_seen_at       INTEGER NOT NULL,
    PRIMARY KEY (kind, drive_id, source_id)
);

CREATE INDEX IF NOT EXISTS idx_crawler_seen_sources_drive
    ON crawler_seen_sources(kind, drive_id, status);
CREATE INDEX IF NOT EXISTS idx_crawler_seen_sources_video
    ON crawler_seen_sources(kind, drive_id, status, canonical_video_id);

-- 去重事务提交后待清理的本地生成资产。数据库状态先原子落地，文件清理
-- 随后幂等执行；进程中断或文件系统临时失败时由下一轮维护继续处理。
CREATE TABLE IF NOT EXISTS duplicate_asset_cleanup_jobs (
    video_id      TEXT PRIMARY KEY,
    preview_local TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_duplicate_asset_cleanup_jobs_updated
    ON duplicate_asset_cleanup_jobs(updated_at, video_id);

-- 网盘账户
CREATE TABLE IF NOT EXISTS drives (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,                -- quark / p115 / p123 / pikpak / wopan / guangyapan / onedrive / googledrive / localstorage / scriptcrawler
    name          TEXT NOT NULL,
    root_id       TEXT NOT NULL DEFAULT '0',
    scan_root_id  TEXT,                          -- deprecated: 扫描起点固定等于 root_id
    credentials   TEXT,                          -- JSON: cookie / refresh_token 等
    status        TEXT DEFAULT 'disconnected',   -- disconnected / ok / error
    last_error    TEXT,
    -- 是否给该盘生成预览视频：1 开 / 0 关。封面生成不受影响。
    -- 替代了早期的全局 preview.enabled 设置（保留旧 setting 行不再读）。
    teaser_enabled INTEGER NOT NULL DEFAULT 1,
    -- 扫描时要跳过的目录 ID 集合（JSON array of string）。命中其中任意一个的目录及其
    -- 全部子目录都不会被递归扫描，也不会进入 SeenFileIDs / VisitedDirIDs 统计。
    -- 替代了早期硬编码"影视"目录的特例分支。
    skip_dir_ids  TEXT NOT NULL DEFAULT '[]',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

-- 扫描任务状态
CREATE TABLE IF NOT EXISTS scans (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    drive_id    TEXT NOT NULL,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    scanned     INTEGER DEFAULT 0,
    added       INTEGER DEFAULT 0,
    error       TEXT
);

-- A provider listing is not authoritative enough to delete catalog data after
-- one empty response. Missing files are confirmed across complete successful
-- scans; seeing the file again clears the counter immediately.
CREATE TABLE IF NOT EXISTS drive_scan_misses (
    drive_id            TEXT NOT NULL,
    file_id             TEXT NOT NULL,
    consecutive_misses  INTEGER NOT NULL DEFAULT 0,
    last_missing_at     INTEGER NOT NULL,
    PRIMARY KEY (drive_id, file_id)
);

CREATE INDEX IF NOT EXISTS idx_drive_scan_misses_threshold
    ON drive_scan_misses(drive_id, consecutive_misses);

CREATE TRIGGER IF NOT EXISTS cleanup_drive_scan_miss_after_video_delete
AFTER DELETE ON videos
WHEN NOT EXISTS (
    SELECT 1 FROM videos WHERE drive_id = OLD.drive_id AND file_id = OLD.file_id
)
BEGIN
    DELETE FROM drive_scan_misses WHERE drive_id = OLD.drive_id AND file_id = OLD.file_id;
END;

CREATE TRIGGER IF NOT EXISTS cleanup_drive_scan_misses_after_drive_delete
AFTER DELETE ON drives
BEGIN
    DELETE FROM drive_scan_misses WHERE drive_id = OLD.id;
END;

-- 管理后台 session（简单 token 存储）
CREATE TABLE IF NOT EXISTS admin_sessions (
    token      TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

-- 一次性视频分享。token_hash 只保存分享令牌摘要；首次领取时原子写入
-- session_hash，此后只有持有对应 HttpOnly cookie 的浏览器可以继续播放。
CREATE TABLE IF NOT EXISTS video_shares (
    id                 TEXT PRIMARY KEY,
    token_hash         TEXT NOT NULL UNIQUE,
    video_id           TEXT NOT NULL,
    created_at         INTEGER NOT NULL,
    consumed_at        INTEGER NOT NULL DEFAULT 0,
    session_hash       TEXT NOT NULL DEFAULT '',
    session_expires_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_video_shares_video
    ON video_shares(video_id);
CREATE INDEX IF NOT EXISTS idx_video_shares_session
    ON video_shares(id, session_hash, session_expires_at);

-- 管理后台登录永久封禁 IP
CREATE TABLE IF NOT EXISTS banned_login_ips (
    ip         TEXT PRIMARY KEY,
    reason     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

-- 全局 key-value 设置（preview 开关等）
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 普通用户表
CREATE TABLE IF NOT EXISTS users (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    username   TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password   TEXT NOT NULL,                    -- bcrypt 哈希
    role       TEXT NOT NULL DEFAULT 'user',     -- admin / user
    banned     INTEGER NOT NULL DEFAULT 0,       -- 1 = 被封禁
    created_at INTEGER NOT NULL
);

-- 短视频随机 feed 会话快照：token → 洗牌后的可见视频 id 列表（JSON 数组）。
-- 持久化让后端重启/更新不再使旧 token 失效，前端可从上次游标续播同一轮。
-- 过期与数量上限由 API 层在写入时清理。
CREATE TABLE IF NOT EXISTS shorts_feed_sessions (
    token       TEXT PRIMARY KEY,
    video_ids   TEXT NOT NULL,
    last_access INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_shorts_feed_sessions_last_access
    ON shorts_feed_sessions(last_access);
