# reach-dlproxy

[简体中文](./README.md) · English

A single-file Go **URL-prefix reverse proxy**: append the target URL to the path and the server fetches it on your behalf, so the client only ever talks to the server. It is the reference implementation of the "video reverse proxy" channel in [Reach](https://github.com/fujioky/reach) — Reach streams X / YouTube video through it or uses it to copy videos into object storage — and works on its own as a large-file download proxy.

```
https://dl.example.com/<password>/https://video.twimg.com/ext_tw_video/.../video.mp4
                       └password┘ └──────────── target URL, verbatim ────────────┘
```

## Using it with Reach

1. Deploy as below and set `DL_PASSWORD`.
2. In Reach, Admin → 系统设置 → 视频反代 → 外部代理, enter `https://dl.example.com/<password>`. Reach requests `<base>/<raw video URL>` and probes `<base>/healthz` (both `/<password>/healthz` and `/healthz` skip auth).
3. Tick "启用故障转移" so playback goes through Reach's own `/api/proxy-video` and the proxy address never reaches visitors' pages.

Upstreams require `Range` requests; the proxy forwards request headers and 206 responses untouched, which is what streaming playback and chunked re-hosting need.

## How it works

Everything after the leading `/` (query included) is parsed as the target URL and handed to `httputil.ReverseProxy`. Around that core:

- **No `http.ServeMux`.** ServeMux cleans paths, collapsing the `//` in `/https://host/...` and issuing a 301 that corrupts the target. Requests are dispatched on the raw path with `http.HandlerFunc`; the post-login redirect writes `Location` by hand instead of `http.Redirect`. Targets already mangled to `https:/host/...` by a slash-merging nginx are tolerated; a missing scheme defaults to `http://`.
- **HTML link rewriting.** HTML responses are buffered and absolute links (`https://<target host>/…`) plus root-relative ones (`href="/…"`, `src="/…"`, `action=`, `content=`, `srcset=`, both quote styles) are rewritten to the proxy prefix. Plain string replacement, no DOM parsing; HTML over 50 MB streams through untouched.
- **Redirect rewriting.** 3xx `Location` is resolved against the current target and prefixed with the proxy origin.
- **Cache policy rewriting.** Upstream `Cache-Control` / `Expires` / `Pragma` / `Surrogate-Control` are dropped. Static content (by Content-Type or URL suffix: pdf, images, css, js, fonts, octet-stream) gets `public, max-age=2592000, immutable` and loses `Set-Cookie` / `Vary`; everything else is `no-store`. The decision is exposed in `X-DL-Cache-Policy`.
- **Connection reuse.** One shared `http.Transport` (64 idle connections, 8 per host). Server `WriteTimeout` is 0 so large downloads are not cut off.

## Password

`DL_PASSWORD`. Empty means open to everyone (a WARNING is logged at start). Two ways to present it:

1. **URL prefix** — `/<password>/https://host/...`, for Reach, download managers and other programs. When authenticated this way the public prefix used for rewriting includes `/<password>`, so rewritten links keep working without a login.
2. **Cookie** — a browser opening `/https://host/...` gets a password page (`POST /__dl_login`); on success a one-year `dl_auth` cookie is set.

The cookie holds an HMAC-SHA256 derivative of the password, never the plaintext, so changing the password invalidates all cookies. Comparison uses `subtle.ConstantTimeCompare`; a wrong password sleeps 500 ms; non-browser clients (no `text/html` in Accept) get a bare 401; the login redirect only accepts same-site paths.

## Endpoints

| Path | Purpose |
| --- | --- |
| `/health`, `/healthz` | JSON health check (status / uptime / time), no auth |
| `/__dl_login` | password form target (POST) |
| anything else | treated as a target URL |

## Environment

| Variable | Default | Meaning |
| --- | --- | --- |
| `LISTEN_ADDR` | `127.0.0.1:18080` | listen address |
| `DL_PASSWORD` | empty | access password; empty = no auth |

## Run locally

```bash
go build -o dl-proxy .
DL_PASSWORD=test LISTEN_ADDR=127.0.0.1:18080 ./dl-proxy

curl -s localhost:18080/healthz
curl -sI -H 'Range: bytes=0-99' localhost:18080/test/https://example.com/
```

## Deploying

No third-party dependencies, Go 1.22+, builds in seconds; a 1-core / 1 GB box is plenty.

**systemd** — `/etc/systemd/system/dl-proxy.service`:

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

Keep the password in a drop-in, `/etc/systemd/system/dl-proxy.service.d/auth.conf` (`Environment=DL_PASSWORD=…` under `[Service]`), out of the main unit and out of the repository.

**Fronting nginx / OpenResty** (TLS termination):

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

Three hard rules:

- **No `proxy_cache`.** A cache layer strips the client's `Range` header and googlevideo answers range-less requests with 403; it also caches 301/302 and `/healthz` for days and makes the password meaningless.
- **`X-Forwarded-Proto` / `X-Forwarded-Host` must be correct** — the proxy builds its public prefix from them; otherwise rewritten links point at `127.0.0.1`.
- With nginx's default `merge_slashes on` the target arrives as `https:/host/...`; the proxy copes, but `merge_slashes off;` in the server block is cleaner.

## Known limits

- Rewriting is string replacement: URLs built by JavaScript, CSS `url()` and links inside inline JSON are not rewritten, so heavy SPAs will not proxy well.
- No host allowlist — whoever has the password can reach any address. The password is the entire security boundary; never expose it unset.
- No log rotation, rate limiting or concurrency cap; be considerate with concurrent large downloads on small machines.
- `proxy error for http://favicon.ico`-style log lines are browsers requesting relative paths that were not rewritten; noise.
