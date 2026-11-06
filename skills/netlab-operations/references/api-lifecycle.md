# API 与生命周期

所有路径相对于控制面的 `/api/v1`。以下请求字段以本技能制作时的契约为基线；执行前按目标版本核对。使用结构化 HTTP 客户端发送 JSON，敏感信息不拼入 shell 命令或 URL。

## 身份与读取

- 外部系统使用 `Authorization: Bearer <service-token>`；浏览器登录使用 `POST /sessions/login`，JSON 为 `{name,password}`，后续保存并发送会话 Cookie。
- `GET /identity` 确定当前身份；账号、环境与资产的授权分开判断。
- `GET /environments` 返回数组，使用契约中的 `cursor`、`limit`、`search` 等参数。
- `GET /environments/{id}` 返回配置、`revision`、`operationId` 等；`GET /environments/{id}/state` 返回运行状态及资产所在节点、实例 ID。
- `GET /operations?environmentId={id}` 查询该环境可见任务；`GET /operations/{id}` 查询执行详情。

## 创建与配置修改

`POST /environments` 使用 `CreateEnvironment`。名称必填；按需求设置 `projectId`、`spec`、`clientRequestId`、`run`，或选择明确的环境模板版本、恢复点、备份来源。

`EnvironmentSpec` 包含 `assets`、`networks`，按需包含 `routers`、`routes`、`policies`、`services`。

修改已存在环境：

1. 读取最新配置与 revision，在其上修改目标字段，保留其余对象及稳定 ID。
2. `POST /environments/{id}/changes`，提交 `{expectedRevision,spec,apply:false}` 查看变更预览。
3. 检查 `effect`、`requiresStop` 和 `dataEffect`，确认替换、停机和数据影响属于当前任务。
4. 使用同一配置与 revision，提交 `apply:true` 和本次 `clientRequestId`，跟踪返回任务。

`PUT /environments/{id}/draft` 保存的是草稿 `{baseRevision,spec}`；正式生效使用 changes。画布展示状态与实际网络配置分别处理。

## 环境与单资产操作

环境入口：`POST /environments/{id}/actions`。
单资产入口：`POST /environments/{id}/assets/{assetId}/actions`。

请求示例，ID 和 revision 由本次查询取得：

```json
{
  "action": "start",
  "expectedRevision": 7,
  "clientRequestId": "<本次逻辑操作的 UUID>"
}
```

契约枚举包含 `start`、`stop`、`force-stop`、`reboot`、`suspend`、`resume`、`rebuild`、`destroy`；环境与单资产的适用动作及允许状态以服务端为准。

| 用户目标 | 操作含义 |
| --- | --- |
| 启动 | 启动现有实例；由平台检查资源与安排执行 |
| 正常关闭 | `stop`；容器先接收镜像停止信号，等待 10 秒后终止剩余进程 |
| 重启 | `reboot`；保留现有系统盘与数据 |
| 暂停、恢复运行 | `suspend`、`resume`；与回滚到恢复点区分 |
| 系统回到初始镜像 | `rebuild`；按当前契约核对数据影响 |
| 一键全面重建 | `rebuild` 配合 `resetVolumes:true`，重建系统盘及环境数据卷；独立持久卷保留并解除挂载 |
| 销毁 | `destroy`；按当前数据保留规则核对运行资源与独立卷 |

强制停止、重建、恢复覆盖和销毁按用户明确的数据范围执行。环境已停止时再启动通常继续使用原运行盘；替换模板引用后若需采用新初始内容，核对 changes 的替换影响或执行明确的重建操作。

## 异步任务与重复请求

- actions 与 changes 应用通常返回 `202` 和 `Operation`，任务位置可从 `Location` 读取。模板导入的 `201` 和更新接口有各自返回结构，分别解析。
- 运行态：`queued`、`running`。终态：`succeeded`、`failed`、`partially_applied`。后两者读取 `phase`、`error`、`results` 核对具体影响。
- 任务 `retryable:true` 时，可对 `POST /operations/{id}/retry` 发起重试，继续跟踪返回任务。
- 遇到 revision 冲突或 `409`，读回最新环境及活动任务，重新评估原变更。请求超时后先查原任务，再决定重试。
- 批量启动优先提交整环境操作，由平台分组与控制并发。多个独立环境的客户端请求采用有限并发，保留每个目标与任务 ID 的对应关系。

## SSE 与上层集成

`GET /environments/{id}/events` 是公共 SSE 入口；外部应用与前端共用。事件是变更通知，对应状态通过查询接口取得。

建立 SSE 连接后读取一次最新配置和 state；将连接建立期间收到的通知合并处理并再次读回对应数据。服务端首次连接默认从当前游标开始，所以初始快照由查询取得。

保存已处理事件的 `id`；断线重连使用 `Last-Event-ID`。收到运行变化查询 state，收到 `security.changed` 查询 security。连接恢复时重新同步当前状态。按需订阅、合并密集通知、限制并发刷新，视图退出后释放连接。

Service Token 客户端使用能发送鉴权头的流式 HTTP 实现；原生浏览器 EventSource 使用已有 Cookie 会话。会话终止或权限变化后按身份状态处理重连。

## 恢复、访问与授权

- 恢复点：`/environments/{id}/recovery-points` 创建或查询，`/{pointId}/restore` 恢复；恢复点与 `rebuild` 的初始镜像来源不同。
- 备份：`/environments/{id}/backups`，配合已配置的 `/backup-repositories` 使用。跨环境独立副本通过 `CreateEnvironment` 的 `recoveryPointId` 或 `backupId` 创建。
- VPN：`/environments/{id}/vpn-access`；暴露服务：`POST /environments/{id}/assets/{assetId}/services`。选择实际接口与网段，使用当前契约的 revision 和请求 ID 字段。
- 权限：`/principals`、`/service-tokens` 和 `/environments/{id}/grants`。按所需项目、环境、资产及动作授权；现有只读凭据继续用于读取，新增授权由用户明确指定。

这些路径的查询、创建、恢复、撤销各有独立 HTTP 方法和请求结构，按目标契约组装；集合路径本身不代表可以直接执行恢复或删除。
