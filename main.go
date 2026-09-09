package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddr = "127.0.0.1:18080"
	staticCacheHeader = "public, max-age=2592000, immutable"
	noStoreHeader     = "no-store"

	// HTML 超过此大小不做改写，直接流式透传，防止撑爆内存
	// 1GB 机器留给其他进程，HTML 改写限制 50MB
	maxHTMLRewriteSize = 50 * 1024 * 1024 // 50 MB
)

// 全局共用 Transport，连接池复用，避免每请求建新连接
var (
	sharedTransport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	startedAt = time.Now()
)

func main() {
	addr := getenv("LISTEN_ADDR", defaultListenAddr)
	password := getenv("DL_PASSWORD", "")
	if password == "" {
		log.Printf("WARNING: DL_PASSWORD not set — proxy is open to everyone")
	}

	// 不用 http.ServeMux：它会清洗路径，把 /https://host/... 里的 "//"
	// 折叠成 "/" 并 301，代理目标 URL 就被改坏了。这里按原始路径手动分发。
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 访问口令，两种给法：
		//   1. URL 前缀   /<口令>/https://host/...   （Reach 配置的反代地址就是这种）
		//   2. cookie     浏览器直接访问 /https://host/... 时弹密码框，输对后种长效 cookie
		// 已经通过 URL 前缀认证时，公开前缀带上 /<口令>，让改写后的跳转/HTML 链接继续免登录。
		proxyBase := externalOrigin(r)
		authorized := password == ""
		if password != "" && stripPasswordPrefix(r, password) {
			authorized = true
			proxyBase = proxyBase + "/" + password
		}

		switch r.URL.Path {
		case "/health", "/healthz":
			handleHealth(w, r)
			return
		case loginPath:
			handleLogin(w, r, password)
			return
		}

		if !authorized && hasAuthCookie(r, password) {
			authorized = true
		}
		if !authorized {
			serveLoginChallenge(w, r, "")
			return
		}
		handleDL(w, r, proxyBase)
	})

	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("dl-proxy listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", noStoreHeader)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"service": "dl-proxy",
		"uptime":  time.Since(startedAt).Round(time.Second).String(),
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

// ── 访问口令 ────────────────────────────────────────────────────────────────

const (
	loginPath        = "/__dl_login"
	authCookieName   = "dl_auth"
	authCookieMaxAge = 365 * 24 * 60 * 60 // 一年
)

// stripPasswordPrefix 检查路径是否以 /<口令>/ 开头；是则原地剥掉前缀并返回 true。
func stripPasswordPrefix(r *http.Request, password string) bool {
	prefix := "/" + password
	path := r.URL.Path
	if path != prefix && !strings.HasPrefix(path, prefix+"/") {
		return false
	}
	// URL 段本身可能含需要转义的字符，用 EscapedPath 保持 RawPath 一致
	escaped := r.URL.EscapedPath()
	escapedPrefix := "/" + url.PathEscape(password)
	r.URL.Path = strings.TrimPrefix(path, prefix)
	if strings.HasPrefix(escaped, escapedPrefix) {
		r.URL.RawPath = strings.TrimPrefix(escaped, escapedPrefix)
	} else {
		r.URL.RawPath = ""
	}
	if r.URL.Path == "" {
		r.URL.Path = "/"
		r.URL.RawPath = ""
	}
	return true
}

// authCookieValue 是口令的 HMAC 派生值——cookie 里不出现口令明文，
// 换口令即让所有旧 cookie 失效。
func authCookieValue(password string) string {
	mac := hmac.New(sha256.New, []byte("dl-proxy-auth"))
	mac.Write([]byte(password))
	return hex.EncodeToString(mac.Sum(nil))
}

func hasAuthCookie(r *http.Request, password string) bool {
	if password == "" {
		return false
	}
	c, err := r.Cookie(authCookieName)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(authCookieValue(password))) == 1
}

// handleLogin 接收密码框提交：口令正确则种 cookie 并跳回原地址。
func handleLogin(w http.ResponseWriter, r *http.Request, password string) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := r.PostFormValue("next")
	// 只允许站内路径，防开放跳转
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	given := r.PostFormValue("password")
	if password == "" || subtle.ConstantTimeCompare([]byte(given), []byte(password)) != 1 {
		time.Sleep(500 * time.Millisecond) // 拖慢暴力尝试
		serveLoginPage(w, next, "口令不正确", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    authCookieValue(password),
		Path:     "/",
		MaxAge:   authCookieMaxAge,
		HttpOnly: true,
		Secure:   externalScheme(r) == "https",
		SameSite: http.SameSiteLaxMode,
	})
	// 不用 http.Redirect：它会清洗路径，把 /https://host 改成 /https:/host
	w.Header().Set("Location", next)
	w.Header().Set("Cache-Control", noStoreHeader)
	w.WriteHeader(http.StatusSeeOther)
}

// serveLoginChallenge 对未认证请求：浏览器给密码框页，其它客户端给 401。
func serveLoginChallenge(w http.ResponseWriter, r *http.Request, msg string) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		serveLoginPage(w, r.URL.RequestURI(), msg, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", noStoreHeader)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func serveLoginPage(w http.ResponseWriter, next, msg string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", noStoreHeader)
	w.WriteHeader(status)
	_ = loginTemplate.Execute(w, map[string]string{"Next": next, "Msg": msg, "Action": loginPath})
}

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow"><title>访问口令</title>
<style>
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font-family:system-ui,-apple-system,sans-serif;background:#f6f7f9;color:#1c1e21}
form{background:#fff;padding:28px 28px 24px;border-radius:12px;box-shadow:0 4px 24px rgba(0,0,0,.08);width:300px}
h1{font-size:17px;margin:0 0 14px}
input[type=password]{width:100%;box-sizing:border-box;padding:10px 12px;border:1px solid #d0d4da;border-radius:8px;font-size:15px}
button{margin-top:12px;width:100%;padding:10px;border:0;border-radius:8px;background:#1c1e21;color:#fff;font-size:15px;cursor:pointer}
p.err{color:#c62828;font-size:13px;margin:10px 0 0}
</style></head><body>
<form method="post" action="{{.Action}}">
<h1>请输入访问口令</h1>
<input type="password" name="password" autofocus autocomplete="current-password" placeholder="口令">
<input type="hidden" name="next" value="{{.Next}}">
<button type="submit">进入</button>
{{if .Msg}}<p class="err">{{.Msg}}</p>{{end}}
</form></body></html>
`))

// handleDL 把请求反代到路径里给出的目标 URL。proxyBase 是对外可见的代理前缀
// （含口令段时形如 https://dl.example.com/<口令>），用于改写跳转和 HTML 链接。
func handleDL(w http.ResponseWriter, r *http.Request, proxyBase string) {
	targetURL, rawTarget, err := parseTarget(r)
	if err != nil {
		http.Error(w, "bad dl target", http.StatusBadRequest)
		return
	}

	publicOrigin := proxyBase

	proxy := &httputil.ReverseProxy{
		Transport: sharedTransport,
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = targetURL.Path
			req.URL.RawPath = targetURL.RawPath
			req.URL.RawQuery = targetURL.RawQuery
			req.Host = targetURL.Host
			req.Header.Set("Host", targetURL.Host)
			req.Header.Set("X-Real-IP", clientIP(r.RemoteAddr))
			req.Header.Set("X-Forwarded-For", forwardedFor(r))
			req.Header.Set("X-Forwarded-Proto", externalScheme(r))
			req.Header.Set("X-Forwarded-Host", externalHost(r))
			req.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			cacheHeader, debugHeader := cachePolicy(
				resp.Header.Get("Content-Type"),
				targetURL.RequestURI(),
			)
			sanitizeProxyHeaders(resp, cacheHeader, debugHeader)

			if isRedirect(resp.StatusCode) {
				if loc := resp.Header.Get("Location"); loc != "" {
					resp.Header.Set("Location", rewriteLocation(loc, targetURL, publicOrigin))
				}
				return nil
			}

			if !isHTML(resp.Header.Get("Content-Type")) {
				return nil
			}

			if resp.ContentLength > maxHTMLRewriteSize {
				return nil
			}

			limited := io.LimitReader(resp.Body, maxHTMLRewriteSize+1)
			body, err := io.ReadAll(limited)
			if err != nil {
				return err
			}
			_ = resp.Body.Close()

			if int64(len(body)) > maxHTMLRewriteSize {
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				return nil
			}

			rewritten := rewriteHTML(body, targetURL, publicOrigin)
			resp.Body = io.NopCloser(bytes.NewReader(rewritten))
			resp.ContentLength = int64(len(rewritten))
			resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			log.Printf("proxy error for %s: %v", rawTarget, err)
			http.Error(rw, "bad gateway", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}

func parseTarget(r *http.Request) (*url.URL, string, error) {
	raw := strings.TrimPrefix(r.URL.RequestURI(), "/")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", errors.New("missing target")
	}
	// 兼容 "https:/host/..."（前置 nginx 合并斜杠、或浏览器缓存的旧 301 落地形式）
	for _, scheme := range []string{"https:/", "http:/"} {
		if strings.HasPrefix(raw, scheme) && !strings.HasPrefix(raw, scheme+"/") {
			raw = scheme + "/" + strings.TrimPrefix(raw, scheme)
			break
		}
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "http://" + raw
	}

	targetURL, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	if targetURL.Scheme == "" || targetURL.Host == "" {
		return nil, "", errors.New("missing scheme or host")
	}
	if targetURL.Path == "" {
		targetURL.Path = "/"
	}
	return targetURL, raw, nil
}

func sanitizeProxyHeaders(resp *http.Response, cacheHeader, debugHeader string) {
	for _, key := range []string{"Cache-Control", "Expires", "Pragma", "Surrogate-Control"} {
		resp.Header.Del(key)
	}
	resp.Header.Set("Cache-Control", cacheHeader)
	resp.Header.Set("X-DL-Cache-Policy", debugHeader)

	if debugHeader == "static-30d" {
		resp.Header.Del("Set-Cookie")
		resp.Header.Del("Vary")
	}
}

func rewriteLocation(location string, current *url.URL, publicOrigin string) string {
	ref, err := url.Parse(location)
	if err != nil {
		return location
	}
	resolved := current.ResolveReference(ref)
	return publicOrigin + "/" + resolved.String()
}

func rewriteHTML(body []byte, current *url.URL, publicOrigin string) []byte {
	proxyPrefix := publicOrigin + "/"
	hostHTTPS := "https://" + current.Host
	hostHTTP := "http://" + current.Host
	upstreamHTTPS := proxyPrefix + "https://" + current.Host
	upstreamHTTP := proxyPrefix + "http://" + current.Host

	replacements := [][2]string{
		{hostHTTPS + "/", upstreamHTTPS + "/"},
		{hostHTTP + "/", upstreamHTTP + "/"},
		{`href="//` + current.Host, `href="` + upstreamHTTPS},
		{`src="//` + current.Host, `src="` + upstreamHTTPS},
		{`action="//` + current.Host, `action="` + upstreamHTTPS},
		{`href="/`, `href="` + upstreamHTTPS + `/`},
		{`src="/`, `src="` + upstreamHTTPS + `/`},
		{`action="/`, `action="` + upstreamHTTPS + `/`},
		{`content="/`, `content="` + upstreamHTTPS + `/`},
		{`srcset="/`, `srcset="` + upstreamHTTPS + `/`},
		{`href='/`, `href='` + upstreamHTTPS + `/`},
		{`src='/`, `src='` + upstreamHTTPS + `/`},
		{`action='/`, `action='` + upstreamHTTPS + `/`},
	}

	rewritten := body
	for _, pair := range replacements {
		rewritten = bytes.ReplaceAll(rewritten, []byte(pair[0]), []byte(pair[1]))
	}
	return rewritten
}

func cachePolicy(contentType, requestURI string) (string, string) {
	lowerType := strings.ToLower(contentType)
	staticTypes := []string{
		"application/pdf",
		"image/",
		"text/css",
		"javascript",
		"font/",
		"application/font",
		"application/octet-stream",
	}
	for _, marker := range staticTypes {
		if strings.Contains(lowerType, marker) {
			return staticCacheHeader, "static-30d"
		}
	}

	lower := strings.ToLower(requestURI)
	staticMarkers := []string{
		"/pdf", ".pdf",
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico",
		".css", ".js", ".woff", ".woff2", ".ttf", ".eot",
	}
	for _, marker := range staticMarkers {
		if strings.Contains(lower, marker) {
			return staticCacheHeader, "static-30d"
		}
	}
	return noStoreHeader, "no-store"
}

func isHTML(contentType string) bool {
	lower := strings.ToLower(contentType)
	return strings.Contains(lower, "text/html") ||
		strings.Contains(lower, "application/xhtml")
}

func externalOrigin(r *http.Request) string {
	return externalScheme(r) + "://" + externalHost(r)
}

func externalScheme(r *http.Request) string {
	if xfp := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xfp != "" {
		return strings.Split(xfp, ",")[0]
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func externalHost(r *http.Request) string {
	if xfh := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); xfh != "" {
		return strings.Split(xfh, ",")[0]
	}
	if r.Host != "" {
		return r.Host
	}
	return "localhost"
}

func forwardedFor(r *http.Request) string {
	existing := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	ip := clientIP(r.RemoteAddr)
	if existing == "" {
		return ip
	}
	return existing + ", " + ip
}

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func isRedirect(code int) bool {
	return code == http.StatusMovedPermanently ||
		code == http.StatusFound ||
		code == http.StatusSeeOther ||
		code == http.StatusTemporaryRedirect ||
		code == http.StatusPermanentRedirect
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
