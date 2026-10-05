# emby-prober

定时测量 Emby 视频下载速度，为 Mihomo 选择节点。镜像：`ghcr.io/zhousiru/emby-prober:latest`，支持 `amd64` / `arm64`。

## 1. 配置 Mihomo

把 [examples/mihomo.yaml](examples/mihomo.yaml) 合并进现有配置，替换节点名称和路由器地址：

```yaml
proxy-groups:
  - name: Emby Prober
    type: select
    proxies: &candidates
      - "你的节点 A"
      - "你的节点 B"
  - name: Emby Probe Test
    type: select
    proxies: *candidates

listeners:
  - name: emby-prober
    type: mixed
    listen: 192.168.50.110
    port: 17891
    proxy: Emby Probe Test
```

启用 controller API 和 secret。将原有 Emby 分组指向 **Emby Prober**；**Emby Probe Test** 只用于测速，不能承载电视流量。两个分组须使用相同的实际节点列表，不要嵌套自动选择分组；也可使用相同的 `use` / `filter` 引用订阅。

daemon 只切换分组选择，不创建分组或修改 Mihomo 配置。controller 和测试端口需允许容器所在主机访问，仅在可信网络开放。容器里 `127.0.0.1` 指向容器自身，请使用实际可达地址。

## 2. 配置并启动

```bash
git clone https://github.com/zhousiru/emby-prober.git
cd emby-prober
cp examples/config.json config.json
cp examples/env.example .env
chmod 600 .env
```

编辑 `config.json` 和 `.env`：

- `emby.url`：API 基地址，如 `https://emby.example.com/emby`。
- `emby.username`：服务器账号；密码填入 `.env` 的 `EMBY_PASSWORD`。
- `emby.item_id`：建议填写实际视频 ID；留空则从最近20个电影/剧集中找合适视频。
- `mihomo.url`：controller API 地址；secret 填入 `.env` 的 `MIHOMO_SECRET`。
- `mihomo.probe_proxy_url`：专用测速端口，如 `http://192.168.50.110:17891`，**必须绑定 Emby Probe Test**，不能填普通代理入口。支持 HTTP/SOCKS5 和 URL 中的代理认证信息。
- `state_dir`：保持 `/state`，Compose 用命名卷保存 token 和结果。

先检查和试测，再启动：

```bash
docker compose run --rm emby-prober --config /config/config.json --check
docker compose run --rm emby-prober --config /config/config.json --once --dry-run
docker compose up -d
docker compose logs -f
```

建议部署在电视所在局域网。正式运行时不要同时启动试测实例。升级用 `docker compose pull && docker compose up -d`。

### 其他 Emby 认证方式

移除 `username` / `password_env`，任选一种，并在 `.env` 添加相应变量：

- **API Key**：设置 `api_key_env: "EMBY_API_KEY"`，同时填写 `user_id`。
- **用户 AccessToken**：设置 `access_token_env: "EMBY_ACCESS_TOKEN"`；未填 `user_id` 时自动查询。

用户名/密码模式会缓存 token，401时重新登录，每轮最多一次；403不会反复登录。API Key / 手动 token 失效后需更新 `.env` 并重建容器。仅支持服务器账号登录，不支持 Emby Connect / SSO。

每次采样重新获取 PlaybackInfo。特殊反向代理可设置 `stream_path: "Videos/{item_id}/original.mkv"`；不要填带 token 的完整 URL。`api_proxy_url` 可指定认证和播放信息请求所用的代理，留空则直连；视频测速始终走专用测试端口。

## 3. 调整测速

`config.json` 中的 `probe`：

| 字段 | 默认 | 说明 |
|---|---|---|
| `rtt_url` | `https://www.gstatic.com/generate_204` | RTT/连通性检查地址，空字符串关闭预检 |
| `rtt_timeout` | `5s` | 每个节点的 RTT 检查超时 |
| `stop_mbps` | `0`（示例为 `100`） | 达到此 Mbps 后停止本轮测速，`0` 表示测完所有可用节点 |
| `interval` | `30m` | 每轮结束后的等待时间 |
| `cron` | 空 | 五字段 cron，可带 `CRON_TZ`；设置后替代 interval，默认时区 UTC |
| `timeout` | `15s` | 每次下载的总时间上限 |
| `max_bytes` | `67108864` | 每次最多64 MiB |
| `min_bytes` | `262144` | 有效样本至少256 KiB |
| `offset` | `1048576` | 从文件1 MiB处开始 |
| `samples` | `2` | 每节点采样次数 |
| `switch_improvement` | `0.2` | 比当前节点快超过20%才切换 |
| `min_hold` | `5m` | 两次切换的最短间隔，当前节点失败时除外 |

例如 `"cron": "CRON_TZ=Asia/Shanghai 0 8,20,22 * * *"` 按北京时间每天 08:00、20:00、22:00 运行。daemon 启动时先测速一次，再等待下个时间点；不会并发执行或补跑错过的时间点。手动执行仍可用 `--once`。

每轮先通过 Mihomo 检查连通性与 RTT（最多4个并发），跳过检查失败的节点，再按 RTT 从低到高逐个下载视频。RTT 只用于筛选和排序，不能代替片源吞吐量；若检测地址在你的网络不可用，请换地址或把 `rtt_url` 设为空字符串。

设置 `stop_mbps: 100` 后：先测当前可用出口，已经达到100 Mbps就保留并结束；否则按 RTT 顺序测其他节点，首个完成全部采样且中位速度达标的节点出现后，不再下载剩余节点。未达标则继续测，直到候选耗尽。提前结束后仍遵守切换阈值和最短保留时间，状态中记录 `stopped_early`、`stop_node` 和 `skipped`。

按包含首字节等待的实际 Mbps 比较，多次采样取中位数，所有采样均有效才可入选；任意一次采样失败就跳过该节点剩余采样。403、断流、错误页面和不支持 Range 的响应无效；全部失败保留当前节点。要测完所有可用节点并选本轮最快，设置 `stop_mbps: 0`、提升阈值为 `0`、保留时间为 `"0s"`。

默认2个节点最大约256 MiB/轮、12 GiB/天；可增加间隔或减小样本。结果保存在状态卷的 `/state/status.json`，日志输出每次测速与选择原因。切换仅影响新连接，现有视频连接不会被主动断开。

## 本地构建

```bash
docker build -t emby-prober .
```

Actions 在推送 `main` 后测试、构建并发布 GHCR `latest` 和 commit SHA 标签；推送 `v*` 标签同时发布版本标签。PR 仅测试构建，不推送镜像。
