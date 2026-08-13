# DEEIX MinIO 存储节点部署任务书

> 交给大容量 VPS 上的 AI/运维人员执行。目标是在**已有 Docker 和 Nginx** 的服务器上部署单节点 MinIO，为 DEEIX-Chat 提供私有 S3 对象存储，并为后续阿里云 Fun-ASR 提供短期预签名 HTTPS 下载 URL。

## 0. 执行原则

- **先检查，后修改**：不要直接覆盖现有 Nginx、Docker Compose、防火墙或证书配置。
- **一次只改一个范围并立即验证**。
- 不输出、截图或提交任何密码、Access Key、Secret Key。
- 不把 Bucket 设置为 public，不创建永久公开文件地址。
- 不删除现有容器、Nginx 站点、证书或数据。
- 禁止运行 `docker compose down -v` 和针对数据目录的递归删除命令。
- 若端口、域名、磁盘挂载点或证书方案不明确，先暂停并向管理员确认。
- 本任务部署的是**单节点 MinIO**：它节省 DEEIX 应用 VPS 的磁盘，但不等于高可用或备份。

## 1. 目标架构

```text
用户上传 MP3
    ↓
DEEIX-Chat 后端
    ↓ S3 API（HTTPS）
大容量 VPS
    ├── Nginx :443
    │     └── storage.<主域名> → 127.0.0.1:9000
    └── MinIO Docker
          ├── S3 API：127.0.0.1:9000
          ├── Console：127.0.0.1:9001（不直接公网开放）
          └── 数据：宿主机大容量磁盘

后续 Fun-ASR：
DEEIX → 生成短期 Presigned GET URL → Fun-ASR 拉取 MP3
```

公网只公开：

- `80/tcp`：证书签发或跳转 HTTPS；
- `443/tcp`：`storage.<主域名>` 的 MinIO S3 API。

不得公开：

- `9000/tcp`；
- `9001/tcp`；
- MinIO Console 公网域名（第一阶段不需要）。

## 2. 开始前必须收集的信息

执行者先报告以下信息，**不得包含密钥**：

1. Linux 发行版和版本；
2. Docker 与 Docker Compose 版本；
3. Nginx 版本、运行方式（宿主机服务还是 Docker）；
4. 当前 `80/443/9000/9001` 监听情况；
5. 大容量磁盘挂载点和剩余空间；
6. 拟使用的子域名，例如 `storage.example.com`；
7. 子域名 DNS 是否已经解析到该 VPS；
8. 现有 HTTPS 证书方案（Certbot、acme.sh、面板或其他）；
9. Nginx 配置目录及已有站点情况；
10. 宿主机 CPU 架构（`amd64`/`arm64`）。

建议的只读检查命令：

```bash
set -euo pipefail
uname -a
cat /etc/os-release
docker version
docker compose version
nginx -v 2>&1 || true
ss -lntp | grep -E ':(80|443|9000|9001)\b' || true
df -hT
```

若 Nginx 在容器中，改为检查对应容器，不要安装第二套 Nginx。

## 3. 管理员需要先完成的 DNS 操作

在 DNS 服务商添加：

```text
类型：A
主机记录：storage
记录值：大容量 VPS 公网 IPv4
代理/CDN：第一轮验证建议关闭，仅 DNS 解析
```

如果使用 IPv6，再单独评估 AAAA 记录；不要在 IPv6 防火墙未配置时盲目添加。

验证：

```bash
getent ahosts storage.example.com
```

必须解析到当前 VPS 公网地址后，才能申请证书和进行公网验证。

## 4. 目录规划

以下是建议值，执行者必须根据真实大容量磁盘挂载点调整：

```text
部署配置：/opt/deeix-minio
对象数据：/data/minio/data
备份目标：不得与 /data/minio/data 位于同一块物理磁盘
```

创建前先验证磁盘：

```bash
set -euo pipefail
df -hT /data
findmnt /data
```

确认 `/data` 确实是大容量磁盘后再执行：

```bash
set -euo pipefail
sudo install -d -m 0750 /opt/deeix-minio
sudo install -d -m 0750 /data/minio/data
```

若实际挂载点不是 `/data`，应修改本文件中的 Compose 挂载路径，不能把大文件误写入系统盘。

## 5. 密钥生成与保存

在 `/opt/deeix-minio` 中创建只允许 root 读取的 `.env`：

```bash
set -euo pipefail
cd /opt/deeix-minio
sudo sh -c 'umask 077
ROOT_USER="minio-root-$(openssl rand -hex 6)"
ROOT_PASSWORD="$(openssl rand -hex 32)"
printf "MINIO_ROOT_USER=%s\nMINIO_ROOT_PASSWORD=%s\n" "$ROOT_USER" "$ROOT_PASSWORD" > .env
chmod 600 .env'
```

要求：

- 不在终端回显 `.env`；
- 不把 `.env` 放进 Git；
- 通过密码管理器或受控 Secret 备份 root 凭证；
- DEEIX 不得使用 root 凭证，后续创建独立最小权限账号。

## 6. Docker Compose

在 `/opt/deeix-minio/docker-compose.yml` 创建以下配置。

> 首次部署前，执行者应从 MinIO 官方镜像仓库确认当前可用的稳定 release tag，并把 `<PINNED_MINIO_RELEASE>` 替换为固定的 `RELEASE.*` 标签。生产环境不要长期使用 `latest`。

```yaml
services:
  minio:
    image: quay.io/minio/minio:<PINNED_MINIO_RELEASE>
    container_name: deeix-minio
    restart: unless-stopped
    env_file:
      - .env
    command: server /data --console-address ":9001"
    ports:
      - "127.0.0.1:9000:9000"
      - "127.0.0.1:9001:9001"
    volumes:
      - /data/minio/data:/data
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1:9000/minio/health/live"]
      interval: 30s
      timeout: 5s
      retries: 5
      start_period: 20s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

如果所选镜像内没有 `curl`，不要让健康检查导致容器永久 unhealthy。可先移除 Compose 的 `healthcheck`，改用宿主机/Nginx 外部监控；变更原因需记录。

校验与启动：

```bash
set -euo pipefail
cd /opt/deeix-minio
sudo docker compose config
sudo docker compose pull
sudo docker compose up -d
sudo docker compose ps
sudo docker compose logs --tail=100 minio
```

本机验证：

```bash
curl -fsS http://127.0.0.1:9000/minio/health/live
```

验证端口只绑定回环地址：

```bash
ss -lntp | grep -E ':(9000|9001)\b'
```

预期为 `127.0.0.1:9000` 和 `127.0.0.1:9001`，不得是 `0.0.0.0` 或 `[::]`。

## 7. Nginx HTTPS 反向代理

### 7.1 不覆盖现有配置

先执行：

```bash
sudo nginx -T > /tmp/nginx-before-minio.txt
```

检查已有配置结构，按该系统的约定新增一个独立站点文件。Ubuntu/Debian 常见路径为：

```text
/etc/nginx/sites-available/storage.example.com
/etc/nginx/sites-enabled/storage.example.com
```

如果使用宝塔、1Panel、Docker Nginx 或自定义配置目录，遵循现有结构，不要另装 Nginx。

### 7.2 HTTPS server 配置

证书签发应复用服务器现有方案。最终 HTTPS 配置应接近：

```nginx
server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name storage.example.com;

    ssl_certificate     /真实证书路径/fullchain.pem;
    ssl_certificate_key /真实证书路径/privkey.pem;

    # MP3 可能较大；应不小于 DEEIX 允许的最大上传值。
    client_max_body_size 1g;

    # S3 上传和长连接需要合理超时。
    proxy_connect_timeout 30s;
    proxy_send_timeout 3600s;
    proxy_read_timeout 3600s;
    send_timeout 3600s;

    # 避免 Nginx 把大文件完整缓冲到 VPS 系统盘。
    proxy_request_buffering off;
    proxy_buffering off;

    location / {
        proxy_set_header Host $http_host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # S3 签名 URL 必须保留原始 URI 和 query string。
        proxy_pass http://127.0.0.1:9000;
    }
}
```

HTTP `80` 的处理方式应与现有证书方案一致。证书就绪后可重定向：

```nginx
server {
    listen 80;
    listen [::]:80;
    server_name storage.example.com;
    return 301 https://$host$request_uri;
}
```

注意：

- 不在 `location /` 上重写路径；
- 不丢弃 query string，否则 Presigned URL 会失效；
- `Host` 必须保持外部域名；
- 不把 `9001` Console 代理到这个域名；
- 不设置 Basic Auth 到 S3 API 域名，否则 Fun-ASR 无法使用纯签名 URL 下载；
- 不通过 Cloudflare 等代理上传大文件，除非已确认套餐上传大小、超时和签名兼容性。

检查并重载：

```bash
sudo nginx -t
sudo systemctl reload nginx
```

如果 Nginx 运行于 Docker，使用该项目既有的配置测试和 reload 方法，不要执行宿主机 `systemctl`。

## 8. HTTPS 与 MinIO 健康验证

```bash
curl -fsS https://storage.example.com/minio/health/live
curl -I https://storage.example.com/
```

预期：

- health endpoint 返回 HTTP 200；
-根路径可能返回 `403 AccessDenied` 或 MinIO XML 错误，这是私有存储的正常表现；
- TLS 证书链有效，域名匹配，无 `-k` 才能成功访问；
- 不能使用自签名证书，因为 Fun-ASR 必须信任 HTTPS 证书。

可进一步检查：

```bash
openssl s_client -connect storage.example.com:443 -servername storage.example.com </dev/null
```

## 9. 创建私有 Bucket 和 DEEIX 专用账号

建议 Bucket：

```text
deeix-files
```

建议应用账号：随机生成，不使用 root。

```bash
set -euo pipefail
cd /opt/deeix-minio
sudo sh -c 'umask 077
ACCESS_KEY="deeix-$(openssl rand -hex 8)"
SECRET_KEY="$(openssl rand -hex 32)"
printf "DEEIX_S3_ACCESS_KEY=%s\nDEEIX_S3_SECRET_KEY=%s\n" "$ACCESS_KEY" "$SECRET_KEY" > .deeix.env
chmod 600 .deeix.env'
```

创建 `/opt/deeix-minio/deeix-policy.json`：

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetBucketLocation",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::deeix-files"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject"
      ],
      "Resource": [
        "arn:aws:s3:::deeix-files/*"
      ]
    }
  ]
}
```

权限：

```bash
sudo chmod 600 /opt/deeix-minio/deeix-policy.json
```

使用 `mc` 容器配置 MinIO。由于 MinIO 只绑定宿主机回环地址，最简单可靠的方式是让 `mc` 使用 host network：

```bash
set -euo pipefail
cd /opt/deeix-minio
sudo docker run --rm \
  --network host \
  --env-file /opt/deeix-minio/.env \
  --env-file /opt/deeix-minio/.deeix.env \
  -v /opt/deeix-minio/deeix-policy.json:/tmp/deeix-policy.json:ro \
  quay.io/minio/mc:latest \
  sh -c '
    set -e
    mc alias set local http://127.0.0.1:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD"
    mc mb --ignore-existing local/deeix-files
    mc admin policy create local deeix-files-policy /tmp/deeix-policy.json || true
    mc admin user add local "$DEEIX_S3_ACCESS_KEY" "$DEEIX_S3_SECRET_KEY" || true
    mc admin policy attach local deeix-files-policy --user "$DEEIX_S3_ACCESS_KEY"
    mc anonymous set none local/deeix-files
    mc stat local/deeix-files
  '
```

对 `|| true` 的要求：执行后必须单独查询并确认最终用户、策略和 Bucket 存在，不能因为忽略错误而假装成功。

建议随后使用 `mc admin user info`、`mc admin policy entities` 等只读命令确认绑定关系。

## 10. 端到端 S3 与 Presigned URL 验证

部署者应使用 DEEIX 专用账号，而不是 root，执行以下验证：

1. 上传一个不含隐私的测试文本或测试 MP3；
2. 读取对象；
3. 生成短期 Presigned GET URL；
4. 从**另一台机器或外部网络**在不附加 Cookie/Authorization Header 的情况下下载；
5. 等待 URL 到期后确认无法继续下载；
6. 删除测试对象；
7. 确认匿名访问 Bucket 仍返回 403。

可使用 AWS CLI、MinIO `mc` 或一个临时 SDK 脚本。Presigned URL 验证要求：

```bash
curl --fail --location --output /tmp/minio-download-test '<PRESIGNED_URL>'
```

不要把完整 Presigned URL 写入公开日志或聊天，因为它在有效期内就是临时访问凭证。

需要特别验证：

- URL 的 host 必须是公网 `storage.example.com`，不能是 `127.0.0.1:9000`；
- URL 使用 HTTPS；
- query string 穿过 Nginx 后签名仍有效；
- 下载不需要登录或额外 Header；
- `Content-Length` 和下载文件哈希一致；
- 该 URL 从 DEEIX VPS 之外也能访问，后续阿里 Fun-ASR 才可能拉取。

## 11. DEEIX-Chat 后续配置值

验证成功后，安全地把以下值交给 DEEIX 部署端（不要写入前端）：

```env
STORAGE_BACKEND=s3
STORAGE_S3_ENDPOINT=https://storage.example.com
STORAGE_S3_REGION=us-east-1
STORAGE_S3_BUCKET=deeix-files
STORAGE_S3_PREFIX=deeix
STORAGE_S3_ACCESS_KEY_ID=<DEEIX_S3_ACCESS_KEY>
STORAGE_S3_SECRET_ACCESS_KEY=<DEEIX_S3_SECRET_KEY>
STORAGE_S3_FORCE_PATH_STYLE=true
```

说明：

- `STORAGE_S3_ENDPOINT` 使用公网 HTTPS 域名；
- `FORCE_PATH_STYLE=true` 与 MinIO 路径风格 URL 配合；
- Bucket 不公开；
- DEEIX 专用账号只有指定 Bucket 的必要权限；
- 不把 MinIO root 账号给 DEEIX；
- DEEIX 当前代码已支持这些 S3 基础配置；
- DEEIX 代码仍需增加 `PresignGetObject` 能力，才能给 Fun-ASR 生成下载 URL。

## 12. Fun-ASR 对接前的验收标准

以下条件必须全部成立：

- [ ] 子域名 DNS 指向大容量 VPS；
- [ ] HTTPS 证书由公共 CA 签发且链完整；
- [ ] MinIO API 仅绑定 `127.0.0.1:9000`；
- [ ] Console 仅绑定 `127.0.0.1:9001`，未公开；
- [ ] Nginx 只代理 S3 API；
- [ ] 数据目录位于确认过的大容量磁盘；
- [ ] `deeix-files` Bucket 为 private；
- [ ] DEEIX 使用独立最小权限账号；
- [ ] 使用 DEEIX 账号可 Put/Get/Delete；
- [ ] Presigned HTTPS URL 可从外部网络无认证下载；
- [ ] Presigned URL 到期后失效；
- [ ] 匿名 Bucket/Object 请求返回 403；
- [ ] Nginx 不把大上传缓冲到系统盘；
- [ ] Docker 日志已设置轮转；
- [ ] 已记录备份方案和恢复责任人；
- [ ] 未在日志、聊天、Git 或截图中泄露任何密钥/签名 URL。

## 13. 备份与容量策略

单节点部署至少需要：

1. 对 `/data/minio/data` 做异机或云端备份；
2. 监控磁盘空间，建议 70% 告警、85% 停止新上传或升级告警；
3. 定期验证备份可恢复，而不只是任务显示成功；
4. 不把唯一备份放在同一 VPS 或同一物理盘；
5. MinIO 数据目录不要直接用普通文件同步工具在服务运行时做不一致复制；优先使用 S3 层面的 `mc mirror` 或经过验证的快照方案；
6. 原始 MP3 的删除生命周期应与 DEEIX 文件删除动作一致；暂不擅自设置自动过期规则。

## 14. 更新与回滚

### 更新前

- 备份 Compose、Nginx 配置和 MinIO 数据；
- 阅读目标 MinIO release notes；
- 固定新镜像 tag；
- 在维护窗口操作。

### 安全停止

```bash
cd /opt/deeix-minio
sudo docker compose down
```

这不会删除 bind-mounted 数据目录。

### 回滚

- 将 Compose 镜像 tag 改回上一固定版本；
- `docker compose pull && docker compose up -d`；
- 验证 health、S3 Put/Get/Delete 和 Presigned URL；
- 若涉及不可逆数据格式升级，必须按 MinIO 官方 release notes 执行，不能盲目降级。

严禁：

```bash
docker compose down -v
rm -rf /data/minio/data
```

## 15. 执行完成后的汇报格式

执行者只汇报非敏感信息：

```markdown
## MinIO 部署结果

- 系统：
- Docker / Compose 版本：
- MinIO 固定镜像 tag：
- 数据磁盘挂载点：
- 数据目录：
- 公网 S3 Endpoint：
- Bucket：deeix-files
- Bucket 是否私有：
- API 端口是否仅绑定 127.0.0.1：
- Console 是否未公开：
- HTTPS 验证：
- S3 Put/Get/Delete 验证：
- 外网 Presigned URL 验证：
- URL 到期验证：
- Nginx 配置检查：
- 备份方案：
- 未解决问题：
```

不要汇报：

- Root 用户名或密码；
- DEEIX Access Key/Secret Key；
- 完整 Presigned URL；
- 含隐私的测试文件内容。

## 16. 与 DEEIX Fun-ASR 方案的接口边界

该存储节点完成后，DEEIX 侧还需要实现：

1. MP3 MIME/扩展名白名单和 `audio` 文件分类；
2. MP3 上传后自动创建异步转写任务；
3. S3 `PresignGetObject`，建议首轮有效期 2 小时；
4. 把签名 HTTPS URL 提交给北京地域 Fun-ASR；
5. 开启 `diarization_enabled`，由模型自动估计说话人数；
6. 轮询任务并立即下载供应商结果；
7. 保存原始结果、标准化句段和 Markdown；
8. 在聊天和 RAG 中使用转写文本；
9. 支持说话人重命名、逐句人工修订和手动重试；
10. 删除 DEEIX 文件时联动删除对象、转写结果和任务记录。

存储节点只负责安全、稳定地保存对象和生成可验证的 S3 访问能力，不保存 DashScope API Key，也不直接调用 Fun-ASR。
