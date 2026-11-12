# 文档索引

安装与使用入口见 [项目说明](../README.md)，后端运行、开发及配置见 [后端文档](../backend/README.md)。

## 使用与配置

- [网盘凭证](DRIVE_CREDENTIALS.md)：凭证获取、表单填写与排错。
- [Telegram 配置](TELEGRAM.md)：启用接入、配置存储及迁移目录。

## 开发与发布

- [自定义前端](CUSTOM_FRONTEND.md)：API、SDK、接入示例与部署约定。
- [前端版本与发布](FRONTEND_RELEASES.md)：安装、切换、包协议和产品打包。
- [爬虫协议](CRAWLER_PROTOCOL.md)：`crawler.v3` 消息格式与脚本模板。
- [CI 与发布工作流](WORKFLOWS.md)：检查、构建、版本发布与失败恢复。

## 运行机制

- [网盘在线播放](CLOUD_PLAYBACK.md)：取链、直连、代理与兼容播放。
- [视频资源生成](RESOURCE_GENERATION.md)：封面、预览和时长任务的取流与恢复。
- [扫盘清理](SCAN_CLEANUP.md)：目录策略、缺失确认与删除边界。
- [去重体系](DEDUP.md)：判定信号、执行时机与数据处理。

面向使用者和开发者的完整说明统一放在此目录；紧贴模块实现的维护记录保留在代码旁，例如 [去重决策记录](../backend/internal/dedupe/RECORDS.md)。
