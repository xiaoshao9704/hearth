// Package webui 把前端构建产物嵌进二进制，让「单文件分发」成立。
// CI 与 Dockerfile 在 go build 前把 web/dist 拷到本包的 dist/（gitignore，git 里只留 .keep）。
// 开发期目录里只有 .keep，Handler 返回 nil，由 main 回落 STATIC_DIR / vite dev server。
package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

func init() {
	// Go 标准库 mime 表没有 .webmanifest，默认会被当成 text/plain 送出；PWA 清单需要 application/manifest+json。
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Handler 返回内嵌前端的静态托管；dist 里没有产物（未拷入）时返回 nil。
func Handler() http.Handler {
	if _, err := dist.Open("dist/index.html"); err != nil {
		return nil
	}
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	return withCacheControl(http.FileServer(http.FS(sub)))
}

// withCacheControl 发版后老页面不能再拿旧壳：index.html 这类不带 hash 的入口不给浏览器
// 做启发式缓存（no-cache = 每次回源校验），带 hash 的 assets/* 内容不变可长缓存。
// 否则旧 index.html 引用的旧 chunk 已被新版删除，动态导入 404，房间页陷入无限重连。
func withCacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}
