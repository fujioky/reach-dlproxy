# reach-dlproxy

简体中文 · [English](./README.en.md)

单文件 Go 写的 **URL 前缀式反向代理**：把目标地址直接拼在路径里访问，由服务器代为取回，客户端只跟服务器说话。它是 [Reach](https://github.com/fujioky/reach) 「视频反代」通道的参考实现：Reach 把 X / YouTube 的视频流经它转发或落盘到对象存储，也可以单独当作大文件下载代理使用。

```
https://dl.example.com/<口令>/https://video.twimg.com/ext_tw_video/.../video.mp4
                       └口令┘ └────────────── 目标 URL 原样拼接 ──────────────┘
```

## 给 Reach 用

1. 按下文部署，设置 `DL_PASSWORD`。
2. Reach 后台 → 系统设置 → 视频反代 → 外部代理，填 `https://dl.example.com/<口令>`。Reach 会按 `<代理地址>/<原始视频 URL>` 拼接请求，并用 `<代理地址>/healthz` 做健康探测（本代理的 `/<口令>/healthz` 与 `/healthz` 都免认证）。
3. 勾选「启用故障转移」，播放请求就会经 Reach 自己的 `/api/proxy-video` 转发，代理地址不会出现在访客页面里。

上游要求 `Range` 请求，本代理原样透传请求头与 206 响应，适合流式播放和分块转存。

## 工作方式

请求路径去掉开头的 `/` 之后，剩下的整段（含 query）被当作目标 URL 解析，交给 `httputil.ReverseProxy` 转发。围绕这个核心做了几件事：

- **不走 `http.ServeMux`。** ServeMux 会清洗路径，把 `/https://host/...` 里的 `//` 折叠成 `/` 再 301，目标 URL 当场被改坏。所以直接用 `http.HandlerFunc` 按原始路径分发；登录后的跳转手写 `Location` 头而不是用 `http.Redirect`。解析目标时兼容被前置 nginx 合并过斜杠的 `https:/host/...` 形式；没有 scheme 的目标默认补 `http://`。
- **HTML 链接改写。** 响应是 HTML 时读进内存，把绝对链接（`https://<目标域>/…`）和根相对链接（`href="/…"`、`src="/…"`、`action=`、`content=`、`srcset=`，单双引号都覆盖）批量替换成带代理前缀的形式。纯字符串替换，不解析 DOM；超过 50 MB 的 HTML 直接流式透传，防止小内存机器被撑爆。
- **重定向改写。** 3xx 的 `Location` 相对当前目标 URL 解析成绝对地址，再套上代理前缀。
- **缓存策略重写。** 上游的 `Cache-Control` / `Expires` / `Pragma` / `Surrogate-Control` 一律删掉换成自己的：按 Content-Type 或 URL 后缀判定静态资源（pdf / 图片 / css / js / 字体 / octet-stream），是则 `public, max-age=2592000, immutable` 并删掉 `Set-Cookie` 与 `Vary`；否则 `no-store`。判定结果写进 `X-DL-Cache-Policy` 响应头。
- **连接池复用。** 全局共享一个 `http.Transport`（64 空闲连接 / 每 host 8 条）。服务端 `WriteTimeout` 为 0，否则大文件下载会被自己掐断。

## 访问口令

`DL_PASSWORD` 环境变量。留空则完全开放（启动时打一行 WARNING）。两种给法：

1. **URL 前缀** — `/<口令>/https://host/...`。适合配给 Reach、下载工具或别的程序。通过前缀认证时，改写用的公开前缀会带上 `/<口令>`，页面里改写出来的链接继续免登录。
2. **Cookie** — 浏览器直接访问 `/https://host/...` 会弹口令页（`POST /__dl_login`），输对后种一年期 cookie `dl_auth`。

cookie 里存的是口令的 HMAC-SHA256 派生值而非明文，换口令即让所有旧 cookie 失效；比对用 `subtle.ConstantTimeCompare`；口令错误 sleep 500ms 拖慢爆破；非浏览器请求（Accept 不含 `text/html`）直接 401；登录跳转只接受站内路径。

## 接口

| 路径 | 说明 |
| --- | --- |
| `/health`、`/healthz` | JSON 健康检查（status / uptime / time），免认证 |
| `/__dl_login` | 口令表单提交端点（POST） |
| 其它一切 | 当作目标 URL 反代 |

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `127.0.0.1:18080` | 监听地址 |
| `DL_PASSWORD` | 空 | 访问口令，留空 = 无认证 |

## 本地运行

```bash
go build -o dl-proxy .
DL_PASSWORD=test LISTEN_ADDR=127.0.0.1:18080 ./dl-proxy

curl -s localhost:18080/healthz
curl -sI -H 'Range: bytes=0-99' localhost:18080/test/https://example.com/   # URL 前缀口令 + Range
```

## 部署

无第三方依赖，Go 1.22+，`go build` 几秒完成，1 核 1 GB 的机器足够。

**systemd** — `/etc/systemd/system/dl-proxy.service`：

```ini
[Unit]
Description=dl-proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/dl-proxy
Environment=LISTEN_ADDR=127.0.0.1:18080
ExecStart=/opt/dl-proxy/dl-proxy
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

口令放在 drop-in `/etc/systemd/system/dl-proxy.service.d/auth.conf`（`[Service]` 下 `Environment=DL_PASSWORD=…`），不进主 unit、不进仓库。

**前置 nginx / OpenResty**（TLS 终止）：

```nginx
location / {
    proxy_pass http://127.0.0.1:18080;
    proxy_http_version 1.1;
    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host  $host;
    proxy_buffering off;
    proxy_request_buffering off;
}
```

三条硬要求：

- **不要开 `proxy_cache`。** 缓存层会剥掉客户端的 `Range` 头，googlevideo 对无 Range 请求直接 403；还会把 301/302 和 `/healthz` 缓存几天，让口令形同虚设。
- **`X-Forwarded-Proto` / `X-Forwarded-Host` 必须传对**，dl-proxy 靠它们拼对外前缀，否则改写出来的链接会指向 `127.0.0.1`。
- 若 nginx 开了 `merge_slashes`（默认开），目标 URL 会变成 `https:/host/...`；dl-proxy 能兼容，但更干净的做法是在 server 块里 `merge_slashes off;`。

## 已知局限

- HTML 改写是字符串替换，JS 运行时拼出的 URL、CSS `url()`、内联 JSON 里的链接不会被改写，重前端的 SPA 基本代理不动。
- 没有域名白名单，拿到口令就能访问任意地址——口令即全部安全边界，不要留空暴露在公网。
- 无日志轮转、无速率限制、无并发上限；小内存机器上大文件并发下载要自己节制。
- 日志里 `proxy error for http://favicon.ico` 一类是浏览器对未改写成功的相对路径发起的请求，属噪声。
