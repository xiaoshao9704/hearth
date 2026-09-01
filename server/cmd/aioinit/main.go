// aioinit：自包含镜像（-livekit / -full 档）的进程编排入口（容器 PID 1）。
// 按环境变量决定拉起哪些内嵌服务，随后启动 hearth 本体：
//
//	EMBED_LIVEKIT=1  拉起 livekit-server（投屏/摄像头内核，端口 7880/7881/7882）
//	EMBED_INGRESS=1  拉起 redis + ingress（OBS WHIP 推流，端口 7888/7885/7886；依赖 EMBED_LIVEKIT）
//
// 各服务端口可经环境变量覆盖（hearth 自身端口沿用 ADDR）：
//
//	LIVEKIT_PORT=7880 LIVEKIT_TCP_PORT=7881 LIVEKIT_UDP_PORT=7882
//	INGRESS_WHIP_PORT=7888 INGRESS_UDP_PORT=7885 INGRESS_TCP_PORT=7886
//
// 其他可选覆盖：
//
//	LIVEKIT_STUN_SERVERS  逗号分隔的 STUN 服务器（默认 STUN 不可达时 livekit 启动即死，国内必配）
//	REDIS_ADDR            ingress 依赖的 redis：host:port 或 redis://[user:pass@]host:port[/db]；空则拉起内嵌 redis
//
// 首次启动在 /data/aio/ 生成随机密钥（持久化，跨重启稳定）；密钥经环境变量喂给
// hearth（走"环境变量固定"语义，管理后台只读）。livekit.yaml / ingress.yaml 每次
// 启动按当前环境变量重新生成（env 权威）——要改端口等参数请改环境变量，手改 yaml
// 会在重启后被覆盖。
// 子进程崩溃退避重启；SIGTERM/SIGINT 广播给全部子进程后退出。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const aioDir = "/data/aio"

func main() {
	log.SetPrefix("[aioinit] ")
	embedLK := os.Getenv("EMBED_LIVEKIT") == "1"
	embedIngress := os.Getenv("EMBED_INGRESS") == "1"
	if embedIngress && !embedLK {
		log.Fatal("EMBED_INGRESS=1 需要 EMBED_LIVEKIT=1（ingress 依赖 livekit-server）")
	}

	key, secret, err := ensureKeys()
	if err != nil {
		log.Fatalf("生成/读取密钥失败: %v", err)
	}

	// 各服务端口均可经环境变量覆盖（默认与上游官方约定一致）
	lkPort := envOr("LIVEKIT_PORT", "7880")
	lkTCPPort := envOr("LIVEKIT_TCP_PORT", "7881")
	lkUDPPort := envOr("LIVEKIT_UDP_PORT", "7882")
	whipPort := envOr("INGRESS_WHIP_PORT", "7888")
	ingUDPPort := envOr("INGRESS_UDP_PORT", "7885")
	ingTCPPort := envOr("INGRESS_TCP_PORT", "7886")
	// 默认 STUN 被墙/不可达时 livekit 启动即死，国内部署必须能换成可用 STUN
	lkStun := os.Getenv("LIVEKIT_STUN_SERVERS")
	// ingress 依赖 redis：REDIS_ADDR 指向外部实例则直接用（支持 redis://[user:pass@]host:port[/db]），空则拉起内嵌 redis
	redisAddr := os.Getenv("REDIS_ADDR")
	embedRedis := embedIngress && redisAddr == ""
	if embedRedis {
		redisAddr = "127.0.0.1:6379"
	}
	redisBlock := ""
	if embedIngress {
		redisBlock = parseRedis(redisAddr).yaml()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var wg sync.WaitGroup

	if embedLK {
		// yaml 每次启动按当前 env 重生成（env 权威），手改会在重启后被覆盖
		if err := os.WriteFile(filepath.Join(aioDir, "livekit.yaml"), []byte(livekitYAML(key, secret, redisBlock, lkPort, lkTCPPort, lkUDPPort, lkStun)), 0o644); err != nil {
			log.Fatalf("写 livekit.yaml 失败: %v", err)
		}
		if embedIngress {
			if err := os.WriteFile(filepath.Join(aioDir, "ingress.yaml"), []byte(ingressYAML(key, secret, lkPort, whipPort, ingUDPPort, ingTCPPort, redisBlock)), 0o644); err != nil {
				log.Fatalf("写 ingress.yaml 失败: %v", err)
			}
			if embedRedis {
				supervise(ctx, &wg, "redis", "/usr/bin/redis-server",
					"--port", "6379", "--bind", "127.0.0.1", "--save", "", "--appendonly", "no")
			}
		}
		supervise(ctx, &wg, "livekit", "/app/livekit-server", "--config", filepath.Join(aioDir, "livekit.yaml"))
		if embedIngress {
			supervise(ctx, &wg, "ingress", "/bin/ingress", "--config", filepath.Join(aioDir, "ingress.yaml"))
		}
	}

	// hearth 本体：内嵌服务的接入参数经 env 固定（管理后台只读展示）
	hearthEnv := os.Environ()
	if embedLK {
		hearthEnv = append(hearthEnv,
			"LIVEKIT_API_URL=http://127.0.0.1:"+lkPort,
			"LIVEKIT_API_KEY="+key,
			"LIVEKIT_API_SECRET="+secret,
		)
	}
	if embedIngress {
		hearthEnv = append(hearthEnv, "INGRESS_UPSTREAM_URL=http://127.0.0.1:"+whipPort)
	}
	superviseEnv(ctx, &wg, "hearth", hearthEnv, "/app/hearth")

	wg.Wait()
	log.Print("全部服务已退出")
}

// ---- 密钥与配置 ----

// ensureKeys 首启生成随机 LiveKit API key/secret 并持久化（/data 卷，跨重启稳定）。
func ensureKeys() (key, secret string, err error) {
	if err := os.MkdirAll(aioDir, 0o755); err != nil {
		return "", "", err
	}
	p := filepath.Join(aioDir, "keys.env")
	if b, err := os.ReadFile(p); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "key="); ok {
				key = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "secret="); ok {
				secret = strings.TrimSpace(v)
			}
		}
		if key != "" && secret != "" {
			return key, secret, nil
		}
	}
	key, secret = "aio_"+randHex(6), randHex(24)
	err = os.WriteFile(p, []byte(fmt.Sprintf("key=%s\nsecret=%s\n", key, secret)), 0o600)
	return key, secret, err
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("随机数失败: %v", err)
	}
	return hex.EncodeToString(b)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func livekitYAML(key, secret, redisBlock, port, tcpPort, udpPort, stunServers string) string {
	stun := ""
	if stunServers != "" {
		var sb strings.Builder
		sb.WriteString("  stun_servers:\n")
		for _, s := range strings.Split(stunServers, ",") {
			sb.WriteString("    - " + strings.TrimSpace(s) + "\n")
		}
		stun = sb.String()
	}
	return fmt.Sprintf(`# aioinit 按环境变量生成，每次重启重写——要改参数请改环境变量，手改不保留
port: %s
rtc:
  udp_port: %s
  tcp_port: %s
  use_external_ip: true
%skeys:
  %s: %s
%s`, port, udpPort, tcpPort, stun, key, secret, redisBlock)
}

func ingressYAML(key, secret, lkPort, whipPort, udpPort, tcpPort, redisBlock string) string {
	return fmt.Sprintf(`# aioinit 按环境变量生成，每次重启重写——要改参数请改环境变量，手改不保留
api_key: %s
api_secret: %s
ws_url: ws://127.0.0.1:%s
%swhip_port: %s
rtmp_port: -1
rtc_config:
  udp_port: %s
  tcp_port: %s
  use_external_ip: true
cpu_cost:
  whip_cpu_cost: 0.3
  whip_bypass_transcoding_cpu_cost: 0.1
`, key, secret, lkPort, redisBlock, whipPort, udpPort, tcpPort)
}

// ---- redis（ingress 依赖）----

// redisCfg 外部 redis 连接参数；REDIS_ADDR 支持 host:port 或 redis://[user:pass@]host:port[/db]
type redisCfg struct{ addr, user, pass, db string }

func parseRedis(raw string) redisCfg {
	if !strings.Contains(raw, "://") {
		return redisCfg{addr: raw}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		log.Fatalf("REDIS_ADDR 解析失败: %q（支持 host:port 或 redis://[user:pass@]host:port[/db]）", raw)
	}
	c := redisCfg{addr: u.Host, db: strings.TrimPrefix(u.Path, "/")}
	if u.User != nil {
		c.user = u.User.Username()
		c.pass, _ = u.User.Password()
	}
	return c
}

// yaml 渲染 livekit/ingress 共用的 redis 配置块；账号密码走单引号防特殊字符破坏 YAML
func (c redisCfg) yaml() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "redis:\n  address: %s\n", c.addr)
	if c.user != "" {
		fmt.Fprintf(&sb, "  username: %s\n", yamlStr(c.user))
	}
	if c.pass != "" {
		fmt.Fprintf(&sb, "  password: %s\n", yamlStr(c.pass))
	}
	if c.db != "" {
		fmt.Fprintf(&sb, "  db: %s\n", c.db)
	}
	return sb.String()
}

func yamlStr(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ---- 子进程管理 ----

func supervise(ctx context.Context, wg *sync.WaitGroup, name, bin string, args ...string) {
	superviseEnv(ctx, wg, name, os.Environ(), bin, args...)
}

// superviseEnv 循环拉起子进程：异常退出按退避重启（1s 起、上限 30s），
// ctx 取消（收到停止信号）时向子进程发 SIGTERM 并结束循环。
func superviseEnv(ctx context.Context, wg *sync.WaitGroup, name string, env []string, bin string, args ...string) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		backoff := time.Second
		for {
			cmd := exec.Command(bin, args...)
			cmd.Env = env
			cmd.Stdout = prefixWriter(name)
			cmd.Stderr = prefixWriter(name)
			start := time.Now()
			if err := cmd.Start(); err != nil {
				log.Printf("%s 启动失败: %v", name, err)
			} else {
				log.Printf("%s 已启动 (pid %d)", name, cmd.Process.Pid)
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				select {
				case <-ctx.Done():
					_ = cmd.Process.Signal(syscall.SIGTERM)
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						_ = cmd.Process.Kill()
						<-done
					}
					return
				case err := <-done:
					log.Printf("%s 退出: %v", name, err)
				}
			}
			// 运行得久说明上次是偶发崩溃，重置退避
			if time.Since(start) > time.Minute {
				backoff = time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
}

// prefixWriter 给子进程输出加 [name] 前缀，便于混流排障。
func prefixWriter(name string) *pw { return &pw{prefix: "[" + name + "] "} }

type pw struct {
	prefix string
	buf    []byte
}

func (w *pw) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		os.Stdout.WriteString(w.prefix + string(w.buf[:i+1]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}
