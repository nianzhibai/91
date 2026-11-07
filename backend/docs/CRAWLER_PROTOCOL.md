# 爬虫协议（crawler.v3）

本文定义项目接受的 `crawler.v3` 爬虫协议。脚本为单个 Python 3 `.py` 文件，按项目命令发现候选视频并解析媒体信息。

## 脚本声明

在脚本前 200 行的模块顶层直接声明字符串，不能放在函数、条件分支内，也不能用表达式拼接：

```python
CRAWLER_NAME = "示例爬虫"  # 必填，去除首尾空白后为 1–80 个字符
CRAWLER_PROTOCOL = "crawler.v3"
CRAWLER_FEEDS = '[{"id":"latest","label":"最新","default":true},{"id":"hot","label":"最热"}]'
```

`CRAWLER_FEEDS` 是单行 JSON 数组字符串，声明脚本支持的抓取栏目，例如最新、最热、推荐。项目静态读取这些声明。

- 每项必填 `id`、`label`，可选布尔值 `default`；不接受额外字段、重复 JSON 键或 `null`。
- `id` 在脚本内唯一且稳定，为 1–64 位 ASCII 字母、数字、下划线或短横线，以字母或数字开头。显示名称变化时保持 ID 不变。
- `label` 为 1–80 个字符，不能含首尾空白或控制字符。
- 必须有且只有一个栏目设置 `default: true`；实际抓取栏目由任务配置的 `feed_id` 指定。
- 最多 64 个栏目，JSON 字符串内容最多 64 KiB；`CRAWLER_FEEDS` 只能声明一次，且位于前 200 行模块顶层。
- 可以省略该声明，此时视为单一栏目 `{"id":"default","label":"默认","default":true}`。

## 启动配置

项目通过以下命令启动脚本：

```bash
python3 crawler.py --job /absolute/path/to/job.json
```

脚本读取 `--job` 指定的 JSON 文件：

```json
{
  "protocol": "crawler.v3",
  "task_id": "本次任务 ID",
  "crawler_id": "爬虫 ID",
  "feed_id": "latest",
  "work_dir": "/absolute/task/work",
  "config": {},
  "network": {"proxy_url": "http://127.0.0.1:7890"}
}
```

| 字段 | 含义 |
| --- | --- |
| `protocol` | 固定为 `crawler.v3` |
| `task_id` | 本次任务 ID |
| `crawler_id` | 爬虫 ID |
| `feed_id` | 本次抓取的栏目 ID，与脚本声明中的某个 `id` 对应；同一任务内保持不变 |
| `work_dir` | 本次任务临时目录的绝对路径，可用于脚本的临时数据 |
| `config` | 站点自定义配置对象，默认 `{}`；脚本应为未配置的参数提供默认值 |
| `network.proxy_url` | HTTP 客户端应使用的代理；未设置时省略该字段 |

配置的代理同时写入 `HTTP_PROXY`、`HTTPS_PROXY` 及其小写环境变量。脚本通过 `feed_id` 选择列表入口，栏目选择不放在 `config` 中。

脚本会被复制到任务目录执行，应保持单文件可运行，不依赖原脚本旁的文件。第三方 Python 库需预先安装在运行项目的环境中。

## 命令与响应

| 通道 | 用途 |
| --- | --- |
| stdin | 逐行读取项目发来的命令 |
| stdout | 只输出协议 JSON，每行一个对象，带换行并立即 `flush` |
| stderr | 输出日志和异常堆栈 |

| 收到的命令 | 成功响应 | 脚本执行内容 |
| --- | --- | --- |
| `discover` | `page` | 返回一批轻量候选和下一页游标 |
| `resolve` | `item` | 解析指定候选，返回标题和媒体信息 |
| `stop` | `stopped` | 确认停止，随后正常退出 |

遵守以下规则：

- 每次处理一个命令，响应原样带回 `request_id`，每个命令只返回一次最终响应。
- 消息使用 UTF-8 JSONL；单行含换行不超过 1 MiB，单次运行 stdout 总量不超过 64 MiB。
- 只使用本文定义的字段和类型，不输出空行、重复 JSON 键或额外字段。可选字段没有值时省略，不填 `null`；响应中只有 `next_cursor` 可以为 `null`，`locator` 内部数据除外。
- 每个命令的 `deadline_at` 是带时区的绝对截止时间。脚本须在此之前完成响应，并按剩余时间设置网络超时。
- 返回结果后继续等待下一条命令；命令之间可能存在较长间隔，不主动输出候选或退出。

项目以实际入库的新增目标和爬取时间控制整轮任务，不限制整轮候选数、解析次数或页数。每次 `discover` 的 `limit` 只限制本次响应大小，不代表整轮额度。默认爬取时限为 3 小时，包含发现、解析、下载、去重和重试等待；达到目标、源站遍历完、达到时限或用户停止时结束获取内容。

每次发现和解析默认最多 5 分钟，其 `deadline_at` 同时受本次操作时限和剩余爬取时间约束，心跳不能延长截止时间。爬取时间用完记录 `time_limit`，单次操作超时记录 `operation_timeout`，具体阶段、目标、新增数量和耗时由项目写入日志。完整下载的视频继续完成入库、资源生成和上传；未完成下载中止并清理临时文件。

## 发现候选：discover → page

收到：

```json
{"type":"discover","request_id":"request-1","cursor":null,"limit":24,"deadline_at":"2026-09-29T08:01:00Z"}
```

回复：

```json
{"type":"page","request_id":"request-1","items":[{"discovery_key":"detail:123","source_id":"123","locator":{"id":"123"}}],"next_cursor":"page:2"}
```

`items` 必须是数组，最多返回 `limit` 条（上限 100）；没有候选时返回 `[]`。每条候选包含：

| 字段 | 要求 |
| --- | --- |
| `discovery_key` | 必填，列表中即可获得的稳定标识，例如详情页路径或站点 ID |
| `source_id` | 可选，源站视频的稳定 ID；列表中能取到时应提供 |
| `locator` | 必填，供 `resolve` 定位内容的 JSON 对象，最多 16 KiB，例如 `{"id":"123"}` |

`discovery_key`、`source_id` 均为 1–512 字节的 UTF-8 字符串，不能有首尾空白或控制字符。同一内容跨次运行应使用相同标识，不要使用临时下载链接、随机数或时间戳作为标识。

同一视频出现在不同栏目时也必须使用相同的稳定标识，不要把栏目 ID 拼入视频身份。

分页要求：

- 首次 `cursor` 为 `null`，后续收到上次返回的 `next_cursor`。
- `next_cursor` 必填；还有候选时返回非空字符串，其长度、空白及控制字符限制与上述标识相同；源站已遍历完时返回 `null`，最后一页仍可包含候选。
- 如果源站一页多于 `limit` 条，用游标记录页面和剩余偏移，下次继续返回，不能截断后丢弃剩余候选。
- 对新的游标推进分页，避免重复游标、重复页面或重复候选。相同游标被重试时应读取同一位置，不依赖隐式自增的全局页码。

发现阶段只读取列表，不要批量请求详情或下载媒体。项目会筛选候选，再逐条发送 `resolve`；不能假定每条候选都会被解析。

## 解析视频：resolve → item

收到：

```json
{"type":"resolve","request_id":"request-2","candidate":{"discovery_key":"detail:123","source_id":"123","locator":{"id":"123"}},"deadline_at":"2026-09-29T08:02:00Z"}
```

回复最小完整视频信息：

```json
{"type":"item","request_id":"request-2","discovery_key":"detail:123","source_id":"123","title":"示例视频","media":{"type":"url","url":"https://example.com/video.mp4"}}
```

除 `type` 和 `request_id` 外，`item` 支持以下字段。字段直接放在响应顶层：

| 字段 | 必填 | 要求 |
| --- | --- | --- |
| `discovery_key` | 是 | 与收到的候选完全一致 |
| `source_id` | 是 | 符合上述标识规则；若候选已提供，不得改变 |
| `title` | 是 | 非空白字符串，最多 4096 字节 |
| `media` | 是 | 视频媒体对象，格式见下文 |
| `thumbnail` | 否 | 封面媒体对象，格式与 `media` 相同 |
| `detail_url` | 否 | 绝对 HTTP(S) 详情页地址，最多 8192 字节，不含用户名和密码 |
| `author` | 否 | 字符串，最多 1024 字节 |
| `duration_seconds` | 否 | 整数秒数，0–604800 |
| `tags` | 否 | 最多 100 个非空白字符串，每个最多 256 字节；只关联项目中已有的标签 |

### 媒体对象

视频和封面均由后端下载，脚本只返回可直接下载的 HTTP(S) 地址（包括 HLS），媒体对象的 `type` 固定为 `url`：

```json
{"type":"url","url":"https://example.com/video.mp4","headers":{"Referer":"https://example.com/detail/123","User-Agent":"Crawler/3"}}
```

- `url` 必填，必须是绝对 HTTP(S) URL，最多 8192 字节，不能内嵌用户名和密码。
- `headers` 可省略；视频与封面的请求头分别填写。Cookie、防盗链 Referer 等下载所需的头也放在这里。
- 最多 64 个请求头；名称须为合法 HTTP 头名，最多 256 字节；值为字符串，最多 8192 字节，不含换行或 NUL。

不支持脚本下载后返回本地文件；媒体对象不接受 `path` 字段或 `file` 类型。

## 错误与重试

`discover` 或 `resolve` 失败时，用 `error` 代替成功响应：

```json
{"type":"error","request_id":"request-2","scope":"source","code":"rate_limited","message":"源站限制访问","retryable":true,"retry_after_seconds":60}
```

| 字段 | 要求 |
| --- | --- |
| `scope` | 必填：`item` 表示单条内容失败，`source` 表示整个源站不可继续；`discover` 只能用 `source` |
| `code` | 必填，仅允许下表中的错误码 |
| `message` | 必填，非空错误说明，最多 8192 字节 |
| `retryable` | 必填布尔值，表示是否允许项目重试 |
| `retry_after_seconds` | 可选整数，0–86400；允许重试时建议等待的秒数，未指定或为 0 时等待 1 秒 |

| 错误码 | 使用场景 |
| --- | --- |
| `not_found` | 内容不存在；不重试 |
| `parse_failed` | 页面或响应结构无法解析 |
| `auth_required` | 需要登录或登录已失效；停止任务，不重试 |
| `rate_limited` | 源站限流 |
| `source_unavailable` | 网络失败或源站暂时不可用 |

项目决定是否重试及等待多久，脚本和 HTTP 库不要额外自动重试。重试使用新的 `request_id`，但游标或候选相同；脚本应能重复执行该操作。单条内容最终失败后可继续其他候选，源站级错误最终失败则停止任务。

## 心跳与停止

需要报告长操作仍在执行时，可在最终响应前发送 `{"type":"heartbeat","request_id":"request-2"}`。心跳必须使用当前请求 ID，且不会延长截止时间。stderr 日志不能代替 `error` 响应。

正常结束时收到：

```json
{"type":"stop","request_id":"request-3","reason":"target_reached","deadline_at":"2026-09-29T08:02:01Z"}
```

无论 `reason` 的值是什么，都回复：

```json
{"type":"stopped","request_id":"request-3"}
```

随后立即以退出码 `0` 退出，不再向 stdout 输出任何内容。停止确认和退出默认须在 1 秒内完成。取消或超时时，进程可能被直接终止，不保证收到 `stop`。

## 完整模板

保存为 `.py` 文件，修改 `CRAWLER_NAME`，并替换 `discover` 和 `resolve` 的站点逻辑。模板仅用 Python 标准库；演示视频地址是占位地址，实际运行需替换为可访问的视频地址。

<!-- crawler-v3-template -->
```python
CRAWLER_NAME = "示例爬虫"
CRAWLER_PROTOCOL = "crawler.v3"
CRAWLER_FEEDS = '[{"id":"latest","label":"最新","default":true},{"id":"hot","label":"最热"}]'

import argparse
import datetime
import json
import sys
import traceback
import urllib.error
import urllib.parse
import urllib.request


class SourceError(Exception):
    def __init__(self, scope, code, message, retryable=False, retry_after=0):
        super().__init__(message)
        self.fields = dict(scope=scope, code=code,
                           message=utf8_prefix(message, 8192) or "操作失败",
                           retryable=retryable,
                           retry_after_seconds=retry_after)


def utf8_prefix(value, max_bytes):
    return str(value).encode("utf-8")[:max_bytes].decode("utf-8", errors="ignore")


def remaining(deadline_at):
    deadline = datetime.datetime.fromisoformat(deadline_at.replace("Z", "+00:00"))
    if deadline.tzinfo is None or deadline.utcoffset() is None:
        raise ValueError("deadline_at 必须包含时区")
    seconds = (deadline - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
    if seconds <= 0:
        raise SourceError("source", "source_unavailable", "操作截止时间已到")
    return min(30, seconds)


def fetch_json(url, deadline_at, scope):
    # 使用后端提供的 HTTP(S)_PROXY；请求失败交给后端决定是否重试。
    try:
        request = urllib.request.Request(url, headers={"User-Agent": "Crawler/3"})
        with urllib.request.urlopen(request, timeout=remaining(deadline_at)) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        code = {401: "auth_required", 403: "auth_required",
                404: "not_found", 429: "rate_limited"}.get(error.code, "source_unavailable")
        error_scope = "source" if code in ("auth_required", "rate_limited") else scope
        raise SourceError(error_scope, code, "HTTP %d" % error.code,
                          code in ("rate_limited", "source_unavailable")) from error
    except json.JSONDecodeError as error:
        raise SourceError(scope, "parse_failed", "源站返回的 JSON 无法解析") from error
    except OSError as error:
        raise SourceError(scope, "source_unavailable", str(error), True) from error


def sample_rows(job):
    """演示数据按栏目筛选；接入站点时改用 feed_id 选择真实列表接口。"""
    rows = job["config"].get("items", [
        dict(id="123", title="最新示例视频", feed="latest", media_url="https://example.com/video.mp4"),
        dict(id="456", title="最热示例视频", feed="hot", media_url="https://example.com/hot.mp4")
    ])
    return [row for row in rows if row.get("feed", "latest") == job["feed_id"]]


def discover(job, cursor, limit, deadline_at):
    """读取所选栏目并分页；同一 cursor 重试时返回相同位置的候选。"""
    remaining(deadline_at)
    rows = sample_rows(job)
    offset = int(cursor or "0")
    selected = rows[offset:offset + limit]
    items = [dict(discovery_key="detail:" + str(row["id"]),
                  source_id=str(row["id"]), locator=dict(id=str(row["id"])))
             for row in selected]
    end = offset + len(selected)
    return dict(items=items, next_cursor=str(end) if end < len(rows) else None)


def resolve(job, candidate, deadline_at):
    """只解析后端指定的这一条候选。"""
    remaining(deadline_at)
    rows = sample_rows(job)
    locator_id = str(candidate["locator"]["id"])
    source_id = candidate.get("source_id", locator_id)
    row = next((row for row in rows if str(row["id"]) == locator_id), None)
    if row is None:
        raise SourceError("item", "not_found", "该内容已不存在")
    return dict(discovery_key=candidate["discovery_key"], source_id=source_id,
                title=row["title"], media=dict(type="url", url=row["media_url"],
                                               headers=row.get("headers", {})))


def main():
    for stream in (sys.stdin, sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8")
    parser = argparse.ArgumentParser()
    parser.add_argument("--job", required=True)
    args = parser.parse_args()
    with open(args.job, encoding="utf-8") as file:
        job = json.load(file)
    job.setdefault("config", {})
    if not isinstance(job["config"], dict):
        raise ValueError("config 必须是对象")
    if job["protocol"] != CRAWLER_PROTOCOL:
        raise ValueError("unsupported protocol")
    if job["feed_id"] not in {feed["id"] for feed in json.loads(CRAWLER_FEEDS)}:
        raise ValueError("unsupported feed_id")
    for line in sys.stdin:
        command = json.loads(line)
        response = dict(request_id=command["request_id"])
        try:
            kind = command["type"]
            if kind == "discover":
                response.update(type="page", **discover(
                    job, command["cursor"], command["limit"], command["deadline_at"]))
            elif kind == "resolve":
                response.update(type="item", **resolve(
                    job, command["candidate"], command["deadline_at"]))
            elif kind == "stop":
                response.update(type="stopped")
            else:
                raise ValueError("unknown command")
        except SourceError as error:
            response.update(type="error", **error.fields)
        except Exception as error:
            traceback.print_exc(file=sys.stderr)
            response.update(type="error", scope="source" if command["type"] == "discover" else "item",
                            code="parse_failed", message=utf8_prefix(error, 8192) or "解析失败", retryable=False)
        print(json.dumps(response, ensure_ascii=False, separators=(",", ":")), flush=True)
        if response["type"] == "stopped":
            return
    raise EOFError("stdin closed before stop")


if __name__ == "__main__":
    main()
```

## 接入站点 API

下面两个函数可替换模板中的 `discover` 和 `resolve`，使用模板的 `fetch_json` 请求函数。将两处默认地址 `https://example.com` 改为实际站点地址；如果 `job.config` 提供了 `base_url`，则优先使用该值。

`locator` 保存请求详情所需的定位参数，`source_id` 表示视频的稳定身份，两者不要求相同。解析时保留候选已经提供的 `source_id`；详情返回的 ID 应与请求使用的定位 ID 对应。以下示例假定详情接口的 `id` 就是该定位 ID，接入实际站点时应按接口格式调整校验。

示例假定站点 API 如下，实际接入时按源站格式调整字段：

| 接口 | 返回格式 |
| --- | --- |
| `GET /api/videos?sort=latest&page=1`（`sort` 支持 `latest`、`hot`） | `{"items":[{"id":"123","detail_url":"https://example.com/detail/123"}],"has_more":true}` |
| `GET /api/videos/123` | `{"id":"123","title":"标题","video_url":"https://example.com/video.mp4"}` |

游标使用“页码:偏移”保留超出 `limit` 的候选。示例要求同一页在任务期间保持稳定；站点若提供快照令牌或稳定游标，应使用该机制分页。

<!-- crawler-v3-site-functions -->
```python
def discover(job, cursor, limit, deadline_at):
    page, offset = map(int, (cursor or "1:0").split(":"))
    base = job["config"].get("base_url", "https://example.com").rstrip("/")
    query = urllib.parse.urlencode({"sort": job["feed_id"], "page": page})
    data = fetch_json(base + "/api/videos?" + query, deadline_at, "source")
    rows = data["items"]
    selected = rows[offset:offset + limit]
    end = offset + len(selected)
    next_cursor = (str(page) + ":" + str(end) if end < len(rows)
                   else str(page + 1) + ":0" if data["has_more"] else None)
    return dict(items=[dict(discovery_key=row["detail_url"], source_id=str(row["id"]),
                            locator=dict(id=str(row["id"]), detail_url=row["detail_url"]))
                       for row in selected], next_cursor=next_cursor)


def resolve(job, candidate, deadline_at):
    base = job["config"].get("base_url", "https://example.com").rstrip("/")
    locator_id = str(candidate["locator"]["id"])
    source_id = candidate.get("source_id", locator_id)
    data = fetch_json(base + "/api/videos/" + urllib.parse.quote(locator_id, safe=""),
                      deadline_at, "item")
    if str(data["id"]) != locator_id:
        raise SourceError("item", "parse_failed", "详情 API 返回了其他视频")
    detail = candidate["locator"]["detail_url"]
    return dict(discovery_key=candidate["discovery_key"], source_id=source_id,
                title=data["title"], detail_url=detail,
                media=dict(type="url", url=data["video_url"], headers={"Referer": detail}))
```
