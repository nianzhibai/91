<p align="center">
  <img width="120" height="120" alt="91" src="https://github.com/user-attachments/assets/5b323c94-bbd3-4dce-bbc8-adc86935b7de" />
</p>

<p align="center">
  😄 个人私有视频站 😄
</p>

## 功能特性

- **多网盘接入** — 支持115、PikPak、123网盘、联通网盘、光鸭网盘、OneDrive、Google Drive、WebDAV等
- **低带宽播放** — 115、PikPak、123网盘、联通网盘、光鸭网盘、OneDrive 支持302模式，播放视频不消耗带宽
- **短视频模式** — 一键切换抖音风格，沉浸刷片
- **视频分享** — 视频支持一次性分享，"看完即焚"
- **爬虫脚本** — 支持导入自定义脚本，但是有一些规范，具体可以参考 [SpiderFor91](https://github.com/Just-Spider/SpiderFor91)

## 预览图
<img src="ReadMeImage/home.webp" alt="首页展示" width="100%" />
<img src="ReadMeImage/player.webp" alt="视频播放页展示" width="100%" />
<img src="ReadMeImage/admin.webp" alt="后台展示" width="100%" />

## 快速开始

### 方式一：一键安装脚本（推荐）

```bash
sudo apt update && sudo apt install -y curl ca-certificates
curl -fsSL https://raw.githubusercontent.com/nianzhibai/91/main/install.sh -o install.sh
sudo bash install.sh
```
部署完成后访问：`http://服务器IP:9191/`

安装后自动注册 `91` 管理命令：
```bash
91            # 打开管理菜单
91 status     # 查看运行状态
91 logs       # 查看日志
91 update     # 更新到最新版本
91 restart    # 重启服务
91 stop       # 停止服务
```
### 方式二：Docker Compose 部署

**1. 准备目录**
```bash
mkdir video-site-91 && cd video-site-91
```
**2. 创建 `docker-compose.yml`**
拉取仓库内置docker-compose.yml：
```bash
curl -fsSL https://raw.githubusercontent.com/nianzhibai/91/main/docker-compose.yml -o docker-compose.yml
```
**3. 启动**
```bash
docker compose up -d
```
**常用命令：**
```bash
docker compose pull && docker compose up -d   # 更新并重启
docker compose logs -f                        # 查看日志
```

## 数据存放位置

### 一键脚本部署
| 路径 | 内容 |
|------|------|
| `/opt/video-site-91/config.yaml` | 配置文件、管理员账号、网盘凭证 |
| `/opt/video-site-91/data/video-site.db` | SQLite 数据库 |
| `/opt/video-site-91/data/previews/` | 封面图和预览片段 |

### Docker Compose 部署
| 路径 | 内容 |
|------|------|
| `./data/config.yaml` | 配置文件、管理员账号、网盘凭证 |
| `./data/video-site.db` | SQLite 数据库 |
| `./data/previews/` | 封面图和预览片段 |

## 其他说明

### 短视频模式
> ios设备不建议使用短视频模式

### 分享链接
> 视频支持生成分享链接，链接只能打开一次，链接分享的视频无需登录即可播放

<img src="ReadMeImage/share.webp" alt="分享页面展示" width="100%" />

### 三屏画面
> 只有竖屏视频支持三屏画面，只有电脑端支持三屏画面，三屏画面播放视频走的是服务器代理

<table>
  <tr>
    <td width="50%"><img src="ReadMeImage/single-screen.webp" alt="单个画面展示" width="100%" /></td>
    <td width="50%"><img src="ReadMeImage/triple-screen.webp" alt="三屏画面展示" width="100%" /></td>
  </tr>
  <tr>
    <td align="center">单屏画面</td>
    <td align="center">三屏画面</td>
  </tr>
</table>

## 使用须知

- **本项目仅面向个人私有部署**
- **请遵守法律**

## 致谢

- [Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) — 参考其页面设计
- [ArtPlayer](https://github.com/zhw2590582/ArtPlayer) — 当前项目使用的视频播放器
- [OpenList](https://github.com/OpenListTeam/OpenList) — 参考其网盘接口
