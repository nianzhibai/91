# 网盘凭证获取与填写指南

适用于当前项目「网盘管理 → 添加网盘 / 编辑凭证」中的全部 11 种存储类型。字段名称、必填条件和自动续期行为已按当前代码核对；外部授权页面核对日期为 2026-11-07。

本文按后台表单说明填写方法。Telegram 使用独立配置入口，见 [配置 Telegram](TELEGRAM.md)。

## 快速选择

| 存储类型 | 推荐获取方式 | 最少需要填写或自动填入的凭证 |
| --- | --- | --- |
| 115 网盘 | 本站扫码登录 | `cookie` |
| 123 网盘 | 本站扫码登录，或账号密码 | 扫码得到 `access_token`，或 `username` + `password` |
| PikPak | 邮箱密码；登录受限时使用刷新令牌 | `username` + `password`，或 `refresh_token` |
| 光鸭网盘 | 本站扫码登录 | `refresh_token`；也支持仅用有效的 `access_token` |
| OneDrive | OpenList API 授权，或自建应用 | `auth_mode` + `refresh_token`；自建应用还需 `client_id` + `client_secret` |
| Google Drive | 自建 OAuth 客户端，通过 OpenList 页面授权 | `client_id` + `client_secret` + `refresh_token` |
| 夸克网盘 | 本站扫码登录 | `cookie` |
| 联通网盘 | 本站扫码登录 | `access_token` + `refresh_token` |
| 移动网盘 | 本站扫码登录 | `authorization` |
| WebDAV | 从 WebDAV 服务获取连接信息 | `base_url` + `username` + `password` |
| 本地存储 | 确认服务端目录位置 | `path` |

## 通用操作

1. 进入「网盘管理」，点击「添加网盘」，选择类型并填写名称。名称可自行设置，例如“我的 115”。
2. 支持扫码的类型可点击「生成二维码」，在手机端完成登录确认，等待本站提示已填入凭证。
3. 首次配置建议将「自定义网盘根目录」留空。需要限定扫描范围时，按后文的根目录说明填写。
4. 点击保存。**扫码成功只会填入表单，保存后才生效。** 确认网盘连接正常，再发起扫描。

表格中的示例均为占位值，不能直接使用。Cookie、令牌和客户端密钥应保存在自己的后台，公开截图或日志时需遮盖。

### 从浏览器复制 Cookie / Authorization

以下以 Chrome / Edge 为例，其他浏览器操作类似：

1. 打开网盘官网并登录自己的账号。
2. 按 `F12` 打开开发者工具，进入「网络 / Network」，选择 `Fetch/XHR`。
3. 刷新网页或打开一个文件夹，选中成功返回文件列表的网盘 API 请求。
4. 在「标头 / Headers → 请求标头 / Request Headers」中找到 `Cookie` 或 `Authorization`，复制完整值。
5. 粘贴时不要带字段名、外层引号或换行。Cookie 保持 `键=值; 键=值` 的一整行格式。

复制的是请求中的 `Cookie`，不要复制响应中的 `Set-Cookie`，也不要把整个请求或 cURL 命令粘进输入框。`document.cookie` 可能漏掉 HttpOnly Cookie，优先使用请求标头。

### 从浏览器复制 refresh_token

刷新令牌通常出现在登录或续期接口的 **响应 / Response** 中，而不是普通文件请求的 Authorization 中。需要捕获登录响应时，先打开 Network 并勾选「保留日志 / Preserve log」，再登录；在响应 JSON 中复制 `refresh_token` 的字符串值，不含引号。

OneDrive、Google Drive 需要走各自的 OAuth 授权流程，详见对应章节。除明确说明外，令牌字段只填原始值，不加 `Bearer `。

## 115 网盘（p115）

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| Cookie（`cookie`） | 是，扫码可自动填入 | 用 115 App 扫本站二维码并确认；手动获取时，在 [115 网页版](https://115.com/) 登录后，从文件列表请求复制完整 Cookie。 |

格式示例：

```text
UID=你的UID; CID=你的CID; SEID=你的SEID; KID=你的KID
```

`UID`、`CID`、`SEID` 必须存在；已有 Cookie 中的 `KID` 也应保留。项目兼容没有 `KID` 的旧 Cookie，但不要自行删除已有字段。Cookie 内的 `CID` 是登录凭证的一部分，不能拿来填写根目录 ID。

失效后重新扫码或复制新的 Cookie 并保存。

## 123 网盘（p123）

支持扫码和账号密码两种方式，完成其中一种即可。扫码时按手机打开的页面提示登录并确认。

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| 手机号/邮箱（`username`） | 账号密码方式必填 | 填写自己在 [123 网盘](https://www.123pan.com/) 使用的手机号或邮箱。 |
| 密码（`password`） | 账号密码方式必填 | 填写网盘登录密码，不是短信验证码或分享提取码。只有验证码登录方式的账号，可先设置登录密码或直接扫码。 |
| `access_token` | 扫码自动获取 | 扫码成功后程序填入并保存，当前表单没有独立输入框，不必手动查找或粘贴。 |

仅扫码登录时，手机号和密码可以留空。令牌失效后，没有账号密码的网盘需要重新扫码；填写了有效账号密码时，程序可在认证失败后尝试重新登录。

## PikPak（pikpak）

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| 邮箱（`username`） | 账号密码方式必填 | 填写 PikPak 登录邮箱，例如 `user@example.com`。 |
| 密码（`password`） | 账号密码方式必填 | 填写 PikPak 自己的账号密码。通过 Google / Apple 等第三方登录时，不要填写第三方账号密码；可使用下面的刷新令牌方式。 |
| `refresh_token` | 刷新令牌方式必填 | 从自己登录 PikPak 网页版后的认证响应获取，只填令牌字符串，不加 `Bearer `。已有该值时，邮箱和密码可以留空。 |

获取刷新令牌：

1. 从 [PikPak 官网](https://mypikpak.com/zh-CN)进入网页版，打开 Network、勾选 Preserve log，再完成登录。
2. 查找 `user.mypikpak.net` 的认证请求。本项目使用 `/v1/auth/signin` 登录、`/v1/auth/token` 续期，可用 `signin` 或 `token` 筛选。
3. 在成功响应中找到 `refresh_token` 并复制。不要误取 `access_token` 或 `captcha_token`；如果当前网页接口变化，以实际登录响应中的字段为准。
4. 将值填入本站的 `refresh_token` 后保存。当前驱动默认使用 Web 平台参数，因此优先从网页版获取。

程序优先使用已有刷新令牌。密码登录遇到 `4126` 风控错误时，可改用刷新令牌；也可在 PikPak 的「设置 → 访问与集成 → WebDAV」创建凭证，再按本文 WebDAV 一节添加。[PikPak 官方 WebDAV 说明](https://mypikpak.com/zh-CN/faq)

## 光鸭网盘（guangyapan）

优先使用本站扫码登录，按手机页面提示完成授权；成功后会同时填入两种令牌。

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| `refresh_token` | 推荐填写；两种令牌至少有一种有效 | 扫码自动填入，或从光鸭登录/续期响应中的同名字段复制原始值，不加前缀。程序用它续期访问令牌。 |
| `access_token` | 有刷新令牌时可留空 | 扫码自动填入，或复制已登录网页请求中的访问令牌。支持 `Bearer eyJ...` 和不带前缀的 `eyJ...` 两种格式。 |

手动获取时，打开 [光鸭官网](https://www.guangyapan.com/)完成登录，在 Network 中查找 `account.guangyapan.com` 的认证响应；本项目续期使用 `/v1/auth/token`。访问令牌也可从 `api.guangyapan.com` 请求的 Authorization 中复制。

只填写有效的 `access_token` 可以连接，但失效后缺少刷新令牌就无法自动续期。两个输入框虽然没有单独标为必填，全部留空仍会导致初始化失败。

## OneDrive（onedrive）

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| 认证方式（`auth_mode`） | 必选，默认 OpenList API | `openlist_api` 使用 OpenList 的应用和续期服务；`custom_app` 使用自己注册的 Microsoft 应用。 |
| 客户端 ID（`client_id`） | 自建应用必填 | Microsoft Entra 应用「概述」中的 **应用程序（客户端）ID**，不是目录（租户）ID。仅自建应用模式显示。 |
| 客户端密钥（`client_secret`） | 自建应用必填 | 应用「证书和密码」中新建客户端密码后得到的 **值 / Value**，不是密钥 ID。仅自建应用模式显示。 |
| `refresh_token` | 是 | 用当前认证方式对应的应用授权后取得，填写完整刷新令牌，不加 `Bearer `。 |

### 方式一：OpenList API

1. 打开 [OpenList Token 工具](https://api.oplist.org/)，选择 OneDrive 国际版，使用 OpenList 提供的应用参数获取 Token。
2. 登录要接入的 Microsoft 账号，完成授权，复制工具返回的刷新令牌。
3. 回到本站，将认证方式设为 **OpenList API**，粘贴到 `refresh_token` 并保存。

此模式无需填写自己的客户端 ID 和密钥，程序会通过 OpenList 服务续期。授权操作可参考 [OpenList OneDrive 文档](https://doc.oplist.org/guide/drivers/onedrive)。

### 方式二：自建应用

1. 打开 [Microsoft Entra 管理中心](https://entra.microsoft.com/)，进入「应用注册 → 新注册」。选择支持自己账号的账户类型；本项目默认使用 `common` 授权端点，可选择支持组织账号和个人 Microsoft 账号的类型。注册后复制应用程序（客户端）ID。[Microsoft 应用注册说明](https://learn.microsoft.com/en-us/entra/identity-platform/quickstart-register-app)
2. 在「身份验证」中添加 **Web** 平台，使用下述 OpenList 工具时，将重定向 URI 设置为 `https://api.oplist.org/onedrive/callback`，与工具使用的地址保持一致。
3. 在「证书和密码 → 客户端密码」中新建密码，立即复制其 **值**。将该值填入本站的 `client_secret`。
4. 在「API 权限」中配置 Microsoft Graph 的**委托权限**。自己的网盘只读可使用 `Files.Read`，需要上传等写操作时使用 `Files.ReadWrite`；涉及更多可访问文件时，根据实际需求选择对应的 `.All` 权限。授权请求还需包含 `offline_access` 才能取得刷新令牌。[Graph 权限说明](https://learn.microsoft.com/en-us/graph/permissions-reference#filesread)、[离线授权说明](https://learn.microsoft.com/en-us/entra/identity-platform/scopes-oidc#the-offline_access-scope)
5. 回到 [OpenList Token 工具](https://api.oplist.org/)，选择 OneDrive 国际版，切换为使用自己的应用参数，填入刚创建的客户端 ID 和密钥，确认回调地址与应用注册一致，再登录并授权，取得刷新令牌。工具实际请求的权限也需要在授权时同意。[工具配置说明](https://doc.oplist.org/guide/drivers/onedrive)
6. 在本站选择 **自建应用**，填入同一应用的 `client_id`、`client_secret`、`refresh_token` 后保存。此模式下后续续期由本站直接请求 Microsoft。

**三项必须属于同一个应用。** 切换认证方式或更换客户端 ID 后，应重新授权取得匹配的刷新令牌。客户端密钥过期后，也需要更新本站保存的密钥值。

当前表单没有区域和 SharePoint 站点配置入口，默认连接国际版 `global` 的账号云盘。上述步骤不能直接套用于世纪互联或 SharePoint 站点；后端虽有相关参数，表单尚未提供填写入口。

## Google Drive（googledrive）

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| 客户端 ID（`client_id`） | 是 | Google Cloud 中 OAuth 客户端的 ID，通常以 `.apps.googleusercontent.com` 结尾。 |
| 客户端密钥（`client_secret`） | 是 | 同一个 OAuth 客户端的 Client secret，复制原始字符串。 |
| `refresh_token` | 是 | 使用上述客户端授权得到的刷新令牌，不是短期 `access_token`。 |
| 网页登录 Cookie（`playback_cookie`） | 否 | 仅网页兼容播放使用。从已登录 `drive.google.com` 的请求标头复制完整 Cookie。 |
| 网页账号索引（`playback_auth_user`） | 否，默认 `0` | 与上述 Cookie 对应的网页账号序号。页面地址含 `/u/3/` 时填 `3`，不填邮箱、账号 ID 或整段 URL。 |

### 获取客户端 ID、密钥和刷新令牌

以下参考 [OpenList Google Drive 文档](https://doc.oplist.org/guide/drivers/google_drive)，使用自己的 OAuth 客户端，通过 OpenList 页面取得令牌。

1. 在 [Google Cloud Console](https://console.cloud.google.com/) 创建或选择项目，启用 **Google Drive API**。
2. 配置 OAuth 同意屏幕的品牌信息和受众；个人使用可选「外部用户」。处于 Testing 状态时，将自己的 Google 邮箱加入测试用户。
3. 创建 **Web 应用** 类型的 OAuth 客户端，将「已获授权的重定向 URI」填写为 `https://api.oplist.org/googleui/callback`，保存客户端 ID 和密钥。
4. 打开 [OpenList 授权页面](https://api.oplist.org/)，选择 **GoogleDrive Login**，填入自己的客户端 ID 和密钥，保持“使用 OpenList 提供的 API”未勾选。
5. 点击「获取授权」，登录目标 Google 账号并同意访问 Drive 文件。自建测试应用若显示“未经 Google 验证”，确认是自己的应用后，可通过「高级」继续授权。
6. 复制授权结果中的 **Refresh Token**，与刚才使用的客户端 ID、密钥一起填入本站并保存。返回的 Access Token 无需手动填写。

三项凭证必须属于同一个 OAuth 客户端。本站后续直接向 Google 续期，OpenList 页面用于获取初始授权；当前表单没有 OpenList 的 `Use online api` 或 `renewapi` 配置项。

外部应用处于 **Testing** 状态且请求了 Drive 权限时，刷新令牌有效期为 7 天。长期使用应按 Google 要求配置发布状态，并在需要时重新授权；是否需要验证取决于应用用途和权限范围。[Google 刷新令牌有效期说明](https://developers.google.com/identity/protocols/oauth2#expiration)

### 可选的网页兼容播放

1. 在浏览器登录 [Google Drive](https://drive.google.com/)，切换到与 OAuth 接入账号一致的账号，并确认可以打开目标视频。
2. 按前文通用方法，从 `drive.google.com` 的已登录请求复制完整 Cookie，填入 `playback_cookie`。当前实现需要 Cookie 中包含 `SAPISID` 或 `__Secure-3PAPISID`，只复制其中一个值不等于完整 Cookie。
3. 多账号登录时，按当前 Drive 页面地址 `/u/数字/` 中的数字填写 `playback_auth_user`；只有一个账号时可留空或填 `0`。

这两项不能替代前面三项 OAuth 凭证。未配置网页 Cookie 时仍可通过 OAuth 扫描及获取原画；配置后才能尝试网页兼容播放，是否可用还取决于上游视频状态。清空 `playback_cookie` 并保存，可以关闭这条兼容播放链路。

## 夸克网盘（quark）

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| Cookie（`cookie`） | 是，扫码可自动填入 | 使用夸克 App 扫本站二维码确认；手动获取时，在 [夸克网盘](https://pan.quark.cn/) 登录，从网盘文件列表请求复制完整 Cookie。 |

手动获取可在 Network 中筛选 `drive.quark.cn`，Cookie 通常包含 `__pus`、`__puus` 等字段：

```text
__pus=你的值; __puus=你的值; 其他字段=其他值
```

保留完整 Cookie。程序会保存上游响应中更新的 Cookie；如果整个登录态已经失效，仍需重新扫码或手动获取。

## 联通网盘（wopan）

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| `access_token` | 是，扫码可自动填入 | 扫本站二维码，在联通网盘 App 中确认；手动方式复制登录结果中的访问令牌原始值，不加 `Bearer `。 |
| `refresh_token` | 是，扫码可自动填入 | 与访问令牌一起获取，填入同一账号、同一次登录返回的刷新令牌。 |

手动获取可参考以下两种位置：

- 先打开 Network 并保留日志，再登录 [联通网盘](https://pan.wo.cn/)，在登录响应中查找 `access_token` / `accessToken` 和 `refresh_token` / `refreshToken`。
- 登录 [联通网盘 H5](https://panservice.mail.wo.cn/h5/wocloud_ai/login)，在「Application / 应用 → Session Storage / 会话存储」中查看登录数据，取访问令牌和刷新令牌的值。

上述手动入口来自 [OpenList 联通网盘说明](https://doc.oplist.org/guide/drivers/wopan)；网页版本变化或找不到明文令牌时，优先使用本站扫码。不要将整个存储 JSON 填进令牌输入框。

扫码结果若带有 `family_id`，程序会一并保存；它不是当前表单的独立输入项。该值为空使用个人空间，有值使用对应家庭空间。后续续期成功时程序会保存新的访问令牌和刷新令牌。

## 移动网盘（yun139）

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| Authorization（`authorization`） | 是，扫码可自动填入 | 用中国移动云盘 App 或微信扫本站二维码，按提示确认；手动方式从 [移动网盘](https://yun.139.com/) 登录后的文件列表请求中复制 Authorization。 |

两种填写格式都支持：

```text
Basic 完整的Base64字符串
```

```text
完整的Base64字符串
```

保留原始编码内容，不要自行解码、重新拼接或替换为 Cookie，也不要加 `Bearer `。如果选中的请求没有 Authorization，刷新目录后换一个已登录的文件 API 请求查看。

程序会在授权仍有效且接近到期时尝试续期；**已经过期的 Authorization 需要重新获取**。

## WebDAV（webdav）

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| WebDAV 地址（`base_url`） | 是 | 从服务提供方或 NAS 的 WebDAV 设置页获取完整服务地址，包含协议和必要路径，例如 `https://openlist.example.com/dav`。 |
| 用户名（`username`） | 是 | 填写该 WebDAV 服务的用户名。服务若单独生成 WebDAV 账号，应使用它提供的值。 |
| 密码（`password`） | 是 | 填写 WebDAV 密码；提供方要求应用专用密码或授权码时，填该专用值。 |

地址必须是后端服务能够访问的 HTTP/HTTPS WebDAV 端点，不能填写网页文件浏览地址或分享链接。用户名、密码分别填写，不要放入 `https://用户名:密码@主机/...`；地址不能带查询参数或 `#` 片段。

「WebDAV根目录」是相对于 `base_url` 的路径。例如地址为 `https://openlist.example.com/dav`，根目录填 `/Movies`，访问位置就是该服务下的 `/dav/Movies`。不要重复填写 `/dav`。

Docker 内的 `localhost` / `127.0.0.1` 指向本站容器本身。连接其他容器或宿主机上的 WebDAV 服务时，填写从本站容器内可达的地址。

## 本地存储（localstorage）

| 字段 | 是否必填 | 获取与填写 |
| --- | --- | --- |
| 本地目录路径（`path`） | 是 | 填写运行后端服务的机器上实际存在的目录，建议使用绝对路径，例如 `/mnt/videos`。该目录应对服务进程可读。 |

这里填写的是服务端目录，不是访问网页的电脑上的目录；也不能填某个视频文件的路径。

Docker 部署时填**容器内路径**。例如宿主机视频位于 `/mnt/videos`，在现有 Compose 的 `91` 服务下保留原有数据挂载并追加：

```yaml
services:
  "91":
    volumes:
      - ./data:/opt/91/data
      - /mnt/videos:/videos:ro
```

应用挂载配置后，本站「本地目录路径」填 `/videos`。本地存储没有独立的自定义网盘根目录输入框，`path` 本身就是扫描根目录。

## 自定义网盘根目录怎么填

根目录是扫描起点，不是登录凭证。云盘通常填写**文件夹 ID**，不要填写分享链接、分享提取码或目录名称；WebDAV 填路径，本地存储使用上面的 `path`。

| 类型 | 留空时使用的根目录 | 指定子目录时填写 |
| --- | --- | --- |
| 115 | `0` | 文件夹 ID；不是登录 Cookie 中的 `CID` |
| 123 | `0` | 文件夹 ID |
| PikPak | 空字符串 | 文件夹 ID |
| 光鸭 | 空字符串 | 文件夹 ID，不填 `/影视` 这样的路径 |
| OneDrive | `root` | Graph 返回的目录 `id`，不填 `/Videos` 或整个网页地址 |
| Google Drive | `root` | 文件夹地址 `/folders/文件夹ID` 中的 ID，去掉后续查询参数 |
| 夸克 | `0` | 文件夹 `fid` |
| 联通 | `0` | 当前空间中的文件夹 ID |
| 移动 | `/` | 文件夹 ID；`/` 表示根目录，不代表支持任意目录路径 |
| WebDAV | `/` | 相对于 WebDAV 地址的路径，如 `/Movies` |
| 本地存储 | 使用 `path` | 直接修改本地目录路径 |

**不知道云盘目录 ID 时，可以从本站统一获取：**

1. 先将根目录留空，保存有效凭证，确认连接成功。
2. 打开浏览器 Network，再进入该网盘详情页的「扫描跳过目录」，展开目标目录的父目录。这里只需展开目录，无需修改跳过状态；如果列表已加载，可刷新详情页重新捕获请求。
3. 在 Network 中筛选 `dirtree`，查看对应请求的 Response。返回项形如 `{"id":"目录ID","name":"目录名"}`，根据名称找到目标文件夹并复制 `id`。
4. 回到「编辑凭证」，把 ID 填入自定义根目录并保存。子目录较深时，逐层展开查找。

## 保存与排错

| 现象 | 检查方法 |
| --- | --- |
| 扫码成功，关闭窗口后仍未连接 | 扫码只填表单；重新打开后完成扫码并点击保存。 |
| 提示“已保存，但 driver 初始化失败” | 配置已经保存，但凭证或连接仍有问题。按具体错误检查令牌、网络和根目录，再编辑保存。 |
| 提示“将在当前网盘任务结束后生效” | 修改已保存，等待当前任务结束后再检查连接状态。 |
| `401`、授权失效、`invalid_grant` | 先检查凭证是否完整、令牌与应用是否匹配；确认失效后重新扫码或授权。 |
| `403` 或权限不足 | 检查账号能否访问该文件夹、OAuth 是否授予所需权限，以及组织账号的授权策略。 |
| `429`、操作频繁 | 等待上游限流恢复，避免连续扫码、保存或反复触发扫描。 |
| Cookie / Authorization 格式错误 | 去掉字段名、引号和换行，保留完整值；检查 Basic / Bearer 前缀是否符合对应网盘要求。 |
| Google OAuth 可用但兼容播放失败 | 检查网页登录 Cookie、网页账号索引及目标账号的视频访问权限；必要时重新复制 Cookie。 |
| 保存成功但扫描不到目标目录 | 先把云盘根目录留空验证连接，再核对目录 ID 和跳过目录设置。 |
| 本地路径不存在 | 检查目录是否在后端运行环境中存在，Docker 是否已挂载，以及服务进程是否有目录权限。 |

编辑已有网盘时，未修改的凭证会保留；大多数凭证字段清空后也会沿用旧值，不能用清空输入框代替退出登录。例外包括 Google Drive 的 `playback_cookie`（清空会关闭网页兼容播放）和 `playback_auth_user`（清空恢复 `0`）。

程序会按各驱动能力保存更新后的 Cookie、Authorization 或令牌，不需要为了续期定时手工改写。自动续期不保证登录态永久有效，上游撤销授权后仍需重新获取。

## 代码依据与字段边界

- [表单字段与默认根目录](https://github.com/nianzhibai/91-web/blob/main/src/admin/drive/constants.ts)、[扫码填充](https://github.com/nianzhibai/91-web/blob/main/src/admin/drive/DriveForm.tsx)、[组合必填校验](https://github.com/nianzhibai/91-web/blob/main/src/admin/drive/credentials.ts)。
- [后端驱动装配](../backend/cmd/server/drives.go)、[凭证保存规则](../backend/internal/api/admin_drive_operations.go)、[各网盘驱动](../backend/internal/drives/)。
- 播放凭证的具体用途见 [网盘在线播放链路](CLOUD_PLAYBACK.md)。

当前表单没有要求用户填写所有后端内部字段。例如 PikPak 的 `access_token`、`captcha_token`、`device_id` 由程序维护，OneDrive 和 Google Drive 的 `access_token` 由刷新令牌换取，联通的 `family_id` 可由扫码返回。光鸭的短信登录参数、OneDrive 的区域及 SharePoint 参数等也未在当前表单开放；不要把其他项目的配置字段直接当作本站输入项。
