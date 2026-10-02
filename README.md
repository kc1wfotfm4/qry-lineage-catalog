# qry-lineage-catalog

把表、字段、作业与上下游依赖的元数据登记成可查询的目录服务，支持按资产类型与责任人筛选、查看直接与间接血缘并追溯字段级来源。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `qry-lineage-catalog.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

## 资产与血缘入口

### `POST /api/v1/assets`

登记或更新一个资产。请求体必须同时包含非空字符串 `id`、`type`、`owner`，以及字符串数组 `upstream`、`downstream`；数组元素必须是已登记资产的 id。同一 `id` 重复登记时更新字段，并在单个事务内全量替换两个依赖列表，旧边不会残留，也读不到部分写入。

- 新资产返回 HTTP 201，重复登记返回 HTTP 200，响应体都是完整资产对象：

```json
{"id":"a","type":"table","owner":"alice","upstream":["u1"],"downstream":["d1"]}
```

### `GET /api/v1/assets`

返回资产数组并按 id 升序。`type` 省略时返回全部资产；提供时仅返回类型完全相等的资产：`/api/v1/assets?type=table`。无匹配时返回空数组 `[]`。

### `GET /api/v1/assets/{id}/upstream`

返回目标 id 与全部可达上游，每个上游对象含 `id`、`type`、`owner`、`upstream`、`downstream`、`distance`；直接依赖距离为 1，间接依赖按最短可达距离计数，同一祖先只出现一次。结果按 distance 升序、同距离按 id 升序：

```json
{"id":"a","upstream":[{"id":"u1","type":"table","owner":"alice","upstream":[],"downstream":[],"distance":1}]}
```

`depth` 省略时返回全部可达上游；显式提供时只接受 1 到 10 的整数，如 `/api/v1/assets/a/upstream?depth=2`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 只写业务原因，不包含 SQL、堆栈或文件路径。

| HTTP | code | 触发场景 |
|---|---|---|
| 400 | `invalid_request` | 请求体不是合法 JSON 或含多余 JSON 值 |
| 400 | `invalid_asset` | 缺少字段、字段类型不符、依赖 id 为空或指向未登记资产 |
| 400 | `invalid_depth` | depth 不是 1 到 10 的整数 |
| 404 | `asset_not_found` | 上游查询的目标资产不存在 |
| 404 | `route_not_found` | 路径不匹配任何路由 |
| 409 | `lineage_cycle` | 登记将造成直接或间接依赖成环；本次写入整体回滚 |
| 503 | `storage_unavailable` | 存储层不可用 |
