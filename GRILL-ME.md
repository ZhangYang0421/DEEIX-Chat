# DEEIX Fun-ASR MVP 决策记录

## 目标

用户在 DEEIX 上传 MP3 或 M4A 后，由后端自动提交阿里云 Fun-ASR。识别完成前附件不能发送；完成后可查看带时间和说话人的文本，并通过 RAG 用于聊天。

## 已确认决策

1. 前端上传单个 MP3 或 M4A，后端自动提交 Fun-ASR；API Key 不暴露给前端。
2. 第一版单个 MP3/M4A 上限 500MB；不安装 `ffprobe`，不做精确时长校验。
3. 每用户总文件配额 10GB。
4. 原始 MP3/M4A 长期保存；删除文件时联动删除对象、转写产物和处理信息。
5. 文件主存储使用大容量 VPS 上的私有 MinIO。
6. MinIO 公网 S3 API 域名为 `storage.369666888.xyz`，通过短期只读 Presigned HTTPS URL 供 Fun-ASR 拉取。
7. MinIO 使用现有 Docker + Nginx 部署；9000/9001 不直接暴露公网。
8. 第一版使用兼容 Endpoint：`https://dashscope.aliyuncs.com/api/v1`。
9. 后端环境变量：`DASHSCOPE_API_KEY`、`DASHSCOPE_BASE_URL`；暂不加入管理后台。
10. 开启说话人分离，由 Fun-ASR 自动估计说话人数。
11. 不新增 `audio_transcription_jobs` 表；复用现有 `file_objects`：
    - `ProcessingStatus`
    - `ProcessingErrorCode`
    - `ProcessingErrorMessage`
    - `ProcessingPayloadJSON`
    - `ExtractStoragePath`
    - `ExtractStatus`
    - `ProcessingStartedAt`
    - `ProcessingCompletedAt`
12. `ProcessingPayloadJSON` 最小持久化 Fun-ASR `task_id`、阶段和三个结果路径，保证服务重启后可以继续轮询。
13. UI 只展示转写中、完成、失败；失败仅提供手动重试，不自动重试。
14. 同一用户重复上传相同 `SHA256 + size` 的 MP3/M4A：成功结果直接复用；处理中复用进度；失败由用户手动重试。
15. 转写完成前附件保留在输入框，但发送按钮禁用；完成后才能发送。
16. 每个文件在 MinIO 保存：
    - `result.raw.json`：供应商原始结果，不允许修改；
    - `transcript.json`：标准化、可编辑结构；
    - `transcript.md`：页面预览和 RAG 文本。
17. 支持把说话人编号统一重命名为姓名或角色，保留原始 `speaker_id`。
18. 支持逐句人工修订；人工文本优先用于展示与 RAG，原始结果永久保留。
19. 新增接口：
    - `GET /files/:file_id/transcript`
    - `PATCH /files/:file_id/transcript`
20. 人工编辑多处后统一保存；PATCH 携带 `revision` 防止并发覆盖，版本冲突返回 HTTP 409。
21. PATCH 只允许修改 `speakerNames` 和已存在句段的 `text`；禁止修改 `segmentID`、`startMs`、`endMs`、原始 `speaker_id`、模型和原始结果。
22. 保存成功后原子更新 `transcript.json` 与 `transcript.md`，再异步替换该文件的 RAG chunks。
23. 转写默认通过 RAG 进入聊天，不在每次请求中注入完整文本。
24. 音频 RAG 按约 2 分钟时间窗口、句子边界切分，相邻窗口约 15 秒重叠；每片保留时间和说话人。
25. 如果聊天上游拒绝当前请求：只标记当前聊天请求失败；保留 MP3/M4A 和转写；不自动重试；允许用户换问题或模型。用户端只显示中性提示，详细外部请求标识只写入受控日志。

## 最小处理流程

```text
POST /files
→ 校验 MP3/M4A MIME/扩展名与 500MB 上限
→ 保存至 MinIO
→ file category = audio
→ enqueue existing file processing queue
→ 生成短期 MinIO Presigned GET URL
→ 提交 Fun-ASR
→ task_id 写入 ProcessingPayloadJSON
→ worker 分阶段轮询（不长时间占用一条 HTTP 请求）
→ 下载原始 JSON
→ 生成 transcript.json + transcript.md
→ 写入 ExtractStoragePath 并标记 ready
→ 按 2 分钟时间窗口建立 RAG chunks
→ 前端解除发送禁用
```

## 内容策略错误的边界

当前访谈期间，外部 Agent Router 多次返回内容策略拒绝。这是当前上游模型/路由对会话内容的判断，不是 Fun-ASR、MinIO 或 DEEIX 文件接口的响应。

实施要求：

- 不把完整录音正文放进系统提示或错误信息；
- 默认只通过 RAG 发送必要片段；
- 不在前端展示外部响应全文；
- 不把签名 URL、密钥或敏感录音正文写入普通日志；
- 人工修订接口不调用 LLM；
- 不能承诺第三方模型永不拒绝请求，只能缩小发送内容并正确隔离失败范围。

## MinIO 验收结果（已通过）

存储节点：`https://storage.369666888.xyz`

- 公共 CA HTTPS 证书链和域名匹配验证通过；health endpoint 无需跳过 TLS 校验即可返回 200；
- `deeix-files` Bucket 为 private，匿名 GET 返回 403；
- DEEIX 独立账号 Put/Get/Delete 已测通；
- Presigned GET 可从公网在无 Cookie、无 Authorization Header 的情况下下载，且内容一致；
- Presigned URL 到期后返回 403；
- Nginx 保留原始 URI/query/Host，签名 URL 经反向代理后仍有效；
- MinIO 9000/9001 仅绑定 `127.0.0.1`，公网只通过 443 访问；
- S3 接入使用 `STORAGE_S3_FORCE_PATH_STYLE=true`；
- 未在本文记录任何 Access Key 或 Secret Key。

唯一剩余集成验收：DEEIX 实现 `PresignGetObject` 后，使用真实 MP3 的短期签名 URL提交一次 Fun-ASR，确认阿里云能实际拉取。

## 待验证

1. Fun-ASR 是否能从 `storage.369666888.xyz` 的短期 Presigned URL 实际拉取 MP3。
2. 单节点 MinIO 的异机备份方案。

## 代码现状与必要修改

- `backend/internal/application/upload/service.go`：MP3 当前落入 `unknown`，需增加 `audio` 分类、MIME 白名单和 500MB 分类上限。
- `backend/internal/infra/objectstore/store.go`：需增加短期 GET 预签名能力。
- `backend/internal/infra/objectstore/s3.go`：使用 AWS SDK `PresignGetObject` 实现。
- `backend/internal/application/processing/service.go`：增加 audio 分支，复用队列但禁用当前文档分支的 3 次自动重试。
- 新增轻量 Fun-ASR client/formatter，不新增任务表。
- 前端 processing 状态轮询需识别 `transcribing`；ready 前禁用发送。
- 新增结构化 transcript 查看/编辑 UI 和 API。
- embedding 增加 audio 时间窗口 chunk builder，不能再被通用 1024-token 分片破坏。

## 下一步

等待 MinIO 部署方返回非敏感验收结果；通过后再制定 TDD 实施计划并开始修改 DEEIX 代码。
