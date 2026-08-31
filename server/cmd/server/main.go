// Hearth 服务端入口：REST API + 聊天 WebSocket + LiveKit 令牌签发。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"

	"hearth/server/internal/api"
	"hearth/server/internal/chat"
	"hearth/server/internal/config"
	"hearth/server/internal/store"

	"golang.org/x/crypto/bcrypt"
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,32}$`)

func main() {
	cfg := config.Load()

	st, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	// CLI 子命令: adduser <用户名> <密码> —— 注册接口默认关闭时由管理员开通账号
	if len(os.Args) > 1 && os.Args[1] == "adduser" {
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "用法: hearth adduser <用户名> <密码>")
			os.Exit(2)
		}
		username, password := os.Args[2], os.Args[3]
		if !usernameRe.MatchString(username) || len(password) < 6 {
			log.Fatal("用户名需 2-32 位字母数字，密码至少 6 位")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			log.Fatalf("密码哈希失败: %v", err)
		}
		u, err := st.CreateUser(context.Background(), username, string(hash))
		if err != nil {
			log.Fatalf("创建用户失败: %v", err)
		}
		fmt.Printf("用户 %s (id=%d) 创建成功\n", u.Username, u.ID)
		return
	}

	hub := chat.NewHub(st, cfg.CORSOrigin)
	a := api.New(st, cfg, hub)

	// chi 路由：API + 聊天 WS + /lk//w/ 反代；具体路由优先于静态通配，无 ServeMux 模式冲突问题
	r := a.Router()
	a.RegisterProxies(r)
	r.Get("/api/chat", hub.ServeHTTP)

	// 可选：静态托管前端构建产物（部署到树莓派时一个二进制搞定）
	if cfg.StaticDir != "" {
		fs := http.FileServer(http.Dir(cfg.StaticDir))
		r.Get("/*", fs.ServeHTTP)
		r.Head("/*", fs.ServeHTTP)
	}

	log.Printf("hearth server 监听于 %s", cfg.Addr)
	log.Fatal(http.ListenAndServe(cfg.Addr, r))
}
