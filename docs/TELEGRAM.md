# 配置 Telegram

原生和 Docker 部署均在网站后台配置，无需额外部署 Bot API 服务。

## 启用 Telegram

1. 向 `@BotFather` 发送 `/newbot`，创建机器人并取得 **Bot Token**。
2. 在 `my.telegram.org` 的 API development tools 获取 **API ID / API Hash**。
3. 打开「配置面板 → Telegram」，填写这三项凭据。需要代理时，在「连接设置」填写网站进程可访问的 HTTP/HTTPS 或 SOCKS5/SOCKS5H 地址；留空直连。
4. 启用并保存，在后台 Telegram 页面点击「测试连接」。
5. 私聊机器人发送 `/id`，将回复的数字填入「允许的用户 ID」并保存。名单为空时不接收视频。
6. 向机器人发送或转发视频，检查接收结果。选填「站点地址」后，机器人回复会附带视频链接。

也可修改 `config.yaml` 中的 `telegram` 配置，保留其他已有配置项：

```yaml
telegram:
  enabled: true
  api_id: 12345
  api_hash: "填写 32 位 API Hash"
  bot_token: "填写 BotFather 提供的 Token"
  allowed_user_ids: [123456789]
  connection_proxy: ""
  site_base_url: "https://video.example.com"
  storage_directory: ""
```

配置保存后自动应用；修改凭据、连接代理或存储目录会自动重连。Docker 中的 `127.0.0.1` 指网站容器自身，代理地址需从容器内可访问。

## 修改存储目录

「配置面板 → Telegram → 下载设置 → 视频与会话目录」对应 `telegram.storage_directory`。填写新目录的绝对路径，例如 `/mnt/telegram`，并确保网站进程有读写权限。留空时使用 `storage.data_dir` 下的 `telegram/`；相对路径按网站进程的工作目录解析。

该目录包含 `library/`（视频）和 `sessions/`（机器人会话）。仅修改配置不会自动迁移文件；已有数据时按以下顺序操作：

1. 停止网站服务，避免下载或转存期间移动文件。
2. 将旧目录的全部内容复制到新目录，保留子目录结构和文件权限，包括 `library/`、`sessions/`。
3. 修改 `config.yaml` 中的 `telegram.storage_directory` 为新目录的绝对路径。一键安装的配置文件默认为 `/opt/91/config.yaml`，源码部署为 `backend/config.yaml`。
4. 启动网站，测试连接，确认旧视频可播放、新视频写入新目录后，再清理旧目录。

尚无旧数据时，可直接在配置面板修改并保存。`sessions/` 包含会话密钥，不要公开或供多个实例同时使用。

### Docker 目录挂载

配置面板填写的是**容器内路径**。例如将宿主机 `/mnt/telegram` 挂载到容器 `/telegram`，在原 Compose 服务中保留现有数据挂载并追加：

```yaml
services:
  "91":
    volumes:
      - ./data:/opt/91/data
      - /mnt/telegram:/telegram
```

将 `telegram.storage_directory` 设为 `/telegram`，运行 `docker compose up -d 91` 应用挂载。已有数据时，先停止容器、将旧目录内容复制到宿主机 `/mnt/telegram/`，再修改配置并启动；默认部署的配置文件位于宿主机 `./data/config.yaml`，旧存储目录为 `./data/telegram/`。
