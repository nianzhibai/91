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
    title            TEXT NOT NULL,
    author           TEXT,
    tags             TEXT,                      -- JSON array
    duration_seconds INTEGER DEFAULT 0,
    size_bytes       INTEGER DEFAULT 0,
    ext              TEXT,
    quality          TEXT,                      -- HD / SD
    thumbnail_url    TEXT,
    thumbnail_status TEXT DEFAULT 'pending',    -- pending / ready / failed / skipped
    thumbnail_failures INTEGER DEFAULT 0,        -- consecutive transient thumbnail generation failures
    preview_file_id  TEXT,                      -- deprecated: 旧版回写网盘后的预览视频 file id
    preview_local    TEXT,                      -- 本地预览视频路径（兜底）
    preview_status   TEXT DEFAULT 'pending',    -- pending / ready / failed
    views            INTEGER DEFAULT 0,
    favorites        INTEGER DEFAULT 0,
    comments         INTEGER DEFAULT 0,
    likes            INTEGER DEFAULT 0,
    dislikes         INTEGER DEFAULT 0,
    category         TEXT,
    hidden           INTEGER DEFAULT 0,          -- 1 = hidden from public display
    frontend_selected INTEGER DEFAULT 0,         -- 1 = explicitly selected for frontend access
    tags_manual      INTEGER DEFAULT 0,          -- 1 = user explicitly curated tags
    badges           TEXT,                      -- JSON array
    description      TEXT,
    published_at     INTEGER NOT NULL,          -- unix ms
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_videos_drive ON videos(drive_id, file_id);
CREATE INDEX IF NOT EXISTS idx_videos_pub   ON videos(published_at DESC);
CREATE INDEX IF NOT EXISTS idx_videos_views ON videos(views DESC);

-- 统一标签池
CREATE TABLE IF NOT EXISTS tags (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    label      TEXT NOT NULL UNIQUE COLLATE NOCASE,
    aliases    TEXT NOT NULL DEFAULT '[]',       -- JSON array
    source     TEXT NOT NULL DEFAULT 'user',     -- system / user / collection / legacy
    frontend_access INTEGER DEFAULT 0,           -- 1 = tag allows frontend access candidates
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS video_tags (
    video_id   TEXT NOT NULL,
    tag_id     INTEGER NOT NULL,
    source     TEXT NOT NULL DEFAULT 'auto',     -- auto / manual / legacy
    created_at INTEGER NOT NULL,
    PRIMARY KEY (video_id, tag_id)
);

CREATE INDEX IF NOT EXISTS idx_video_tags_tag ON video_tags(tag_id);
CREATE INDEX IF NOT EXISTS idx_video_tags_video ON video_tags(video_id);

-- 用户手动删除过的非系统标签。自动扫描/迁移不再重新创建同名标签；
-- 管理员手动新建同名标签时会移除这里的记录。
CREATE TABLE IF NOT EXISTS deleted_tags (
    label      TEXT PRIMARY KEY COLLATE NOCASE,
    source     TEXT NOT NULL DEFAULT '',
    deleted_at INTEGER NOT NULL
);

-- 网盘账户
CREATE TABLE IF NOT EXISTS drives (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,                -- quark / p115 / p123 / pikpak / wopan / onedrive / googledrive / localstorage / spider91
    name          TEXT NOT NULL,
    root_id       TEXT NOT NULL DEFAULT '0',
    scan_root_id  TEXT,                          -- deprecated: 扫描起点固定等于 root_id
    credentials   TEXT,                          -- JSON: cookie / refresh_token 等
    status        TEXT DEFAULT 'disconnected',   -- disconnected / ok / error
    last_error    TEXT,
    -- 是否给该盘生成预览视频/封面：1 开 / 0 关。
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

-- 管理后台 session（简单 token 存储）
CREATE TABLE IF NOT EXISTS admin_sessions (
    token      TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

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
