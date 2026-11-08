<p align="center">
  <img width="120" height="120" alt="91" src="https://github.com/user-attachments/assets/5b323c94-bbd3-4dce-bbc8-adc86935b7de" />
</p>

<p align="center">
  😄 个人私有视频站 😄
</p>

## 功能特性

- **多网盘接入** — 支持夸克、115、PikPak、123 网盘、联通网盘、移动网盘、光鸭网盘、OneDrive、Google Drive等
- **低带宽播放** — 部分网盘支持直链播放，降低服务器带宽占用
- **短视频模式** — 一键切换抖音风格，沉浸刷片
- **视频分享** — 视频支持一次性分享，"看完即焚"
- **爬虫脚本** — 支持导入自定义脚本，可参考 [SpiderFor91](https://github.com/Just-Spider/SpiderFor91)
- **接入TG** — 支持接入 Telegram，具体可查看 [接入指南](docs/TELEGRAM.md)

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

安装后会自动注册 `91` 管理命令：

```bash
91                  # 打开管理菜单
91 start            # 启动服务
91 stop             # 停止服务
91 restart          # 重启服务
91 update           # 更新到最新版本
91 status           # 查看运行状态
91 reset-password   # 重置密码
91 logs             # 查看服务日志
```

### 方式二：Docker 部署

```bash
mkdir -p 91 && cd 91
curl -fsSL https://raw.githubusercontent.com/nianzhibai/91/main/91-docker.sh -o 91-docker.sh
bash 91-docker.sh install
```

部署完成后访问 `http://服务器IP:9191/`

其他管理命令：

```bash
bash 91-docker.sh update           # 更新镜像并重建容器
bash 91-docker.sh reset-password   # 重置用户密码
docker compose logs -f            # 查看日志
```

## 数据默认存放位置

| 数据类型 | 一键脚本部署 | Docker Compose 部署 |
| --- | --- | --- |
| 运行配置 | `/opt/91/config.yaml` | `<部署目录>/data/config.yaml` |
| 数据库 | `/opt/91/data/91.db` | `<部署目录>/data/91.db` |
| Telegram 视频 | `/opt/91/data/telegram/library/` | `<部署目录>/data/telegram/library/` |
| 数据目录 | `/opt/91/data/` | `<部署目录>/data/` |

目录配置和迁移说明见 [配置与数据](backend/README.md#配置与数据)、[旧部署升级](backend/README.md#旧部署升级)

## 其他说明

完整指南与机制说明见 [文档索引](docs/README.md)

源码部署、开发运行及 GPU 配置见 [后端文档](backend/README.md)，各网盘的直连与代理行为见 [在线播放链路](docs/CLOUD_PLAYBACK.md)

### 分享链接

> 链接首次成功领取后即失效，领取者无需登录即可播放该视频

<img src="ReadMeImage/share.webp" alt="分享页面展示" width="100%" />

### 三屏画面

> 三屏画面仅支持电脑端的竖屏视频（播放通过服务器代理）

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
- **请遵守法律法规**

## 致谢

- [Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) — 参考页面设计
- [ArtPlayer](https://github.com/zhw2590582/ArtPlayer) — 项目使用的播放器
- [td](https://github.com/gotd/td) — 项目使用的 Telegram 客户端库
- [OpenList](https://github.com/OpenListTeam/OpenList) — 参考部分网盘接口

## 捐赠

如果这个项目对你有帮助，欢迎请我喝杯咖啡

<table>
  <tr>
    <td width="50%"><img src="ReadMeImage/donate-wechat.webp" alt="微信" width="100%" /></td>
    <td width="50%"><img src="ReadMeImage/donate-alipay.webp" alt="支付宝" width="100%" /></td>
  </tr>
  <tr>
    <td align="center">微信</td>
    <td align="center">支付宝</td>
  </tr>
</table>

## 赞助者

感谢支持，你的支持是我前进的动力💗

<table>
  <tr>
    <td align="center" width="120">
      <img src="ReadMeImage/sponsors/sponsor001.png" alt="*帅的头像" width="80" height="80" /><br />
      <sub>*帅</sub>
    </td>
  </tr>
</table>
