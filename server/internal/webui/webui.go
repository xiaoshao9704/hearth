// Package webui 把前端构建产物嵌进二进制，让「单文件分发」成立。
// CI 与 Dockerfile 在 go build 前把 web/dist 拷到本包的 dist/（gitignore，git 里只留 .keep）。
// 开发期目录里只有 .keep，Handler 返回 nil，由 main 回落 STATIC_DIR / vite dev server。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var dist embed.FS

// Handler 返回内嵌前端的静态托管；dist 里没有产物（未拷入）时返回 nil。
func Handler() http.Handler {
	if _, err := dist.Open("dist/index.html"); err != nil {
		return nil
	}
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	return http.FileServer(http.FS(sub))
}
