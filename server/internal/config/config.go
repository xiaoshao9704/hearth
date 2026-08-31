// 配置加载：环境变量优先，附带简易 .env 解析（不覆盖已有环境变量）。
package config

import (
	"bufio"
	"os"
	"strings"
)

type Config struct {
	Addr               string // HTTP 监听地址
	DBPath             string // sqlite 文件路径（DATABASE_URL 为空时使用）
	DatabaseURL        string // 数据库连接串：空/sqlite = sqlite，mysql:// 或 postgres:// 切换后端
	LiveKitURL         string // LiveKit WSS 地址（透传给前端）；空 = 按请求推导同源 /lk 代理地址
	LiveKitKey         string // LiveKit API Key
	LiveKitSecret      string // LiveKit API Secret
	LiveKitAPIURL      string // LiveKit server Twirp API 地址（也是 /lk/ 代理上游）
	IngressPublicURL   string // WHIP 公开基地址；空 = 按请求推导同源 /w/ 代理地址
	IngressUpstreamURL string // WHIP 上游（/w/ 代理目标）；空 = Ingress 未启用（/api/ingress 返回 503）
	CORSOrigin         string // 允许的跨域来源，* 表示全部
	StaticDir          string // 可选：前端构建产物目录，空则不托管
	RegOpen            bool   // 兼容旧变量 REGISTRATION_OPEN=true（等价 REG_POLICY=open）
	RegPolicy          string // 注册策略默认值：closed / invite / open（可被后台设置覆盖）
	PublicURL          string // 站点公开地址（拼邀请链接用）；空 = 按请求推导
}

func Load() Config {
	loadDotEnv(".env")
	livekitURL := env("LIVEKIT_URL", "")
	return Config{
		Addr:               env("ADDR", ":8080"),
		DBPath:             env("DB_PATH", "hearth.db"),
		DatabaseURL:        env("DATABASE_URL", ""),
		LiveKitURL:         livekitURL,
		LiveKitKey:         env("LIVEKIT_API_KEY", ""),
		LiveKitSecret:      env("LIVEKIT_API_SECRET", ""),
		LiveKitAPIURL:      env("LIVEKIT_API_URL", deriveAPIURL(livekitURL)),
		IngressPublicURL:   env("INGRESS_PUBLIC_URL", ""),
		IngressUpstreamURL: env("INGRESS_UPSTREAM_URL", ""),
		CORSOrigin:         env("CORS_ORIGIN", "*"),
		StaticDir:          env("STATIC_DIR", ""),
		RegOpen:            env("REGISTRATION_OPEN", "") == "true",
		RegPolicy:          env("REG_POLICY", ""),
		PublicURL:          env("PUBLIC_URL", ""),
	}
}

// DefaultRegPolicy 注册策略默认值：REG_POLICY 优先，兼容 REGISTRATION_OPEN=true，否则邀请制。
func (c Config) DefaultRegPolicy() string {
	switch c.RegPolicy {
	case "closed", "invite", "open":
		return c.RegPolicy
	}
	if c.RegOpen {
		return "open"
	}
	return "invite"
}

// DatabaseDSN 数据库连接串：DATABASE_URL 优先，空则回退 DB_PATH（sqlite 文件路径）。
func (c Config) DatabaseDSN() string {
	if c.DatabaseURL != "" {
		return c.DatabaseURL
	}
	return c.DBPath
}

// deriveAPIURL 从 WSS 地址推导 Twirp API 地址：wss→https、ws→http、去路径。
func deriveAPIURL(wss string) string {
	if wss == "" {
		return "http://localhost:7880"
	}
	u := strings.TrimSuffix(wss, "/")
	u = strings.TrimPrefix(u, "wss://")
	u = strings.TrimPrefix(u, "ws://")
	scheme := "http"
	if strings.HasPrefix(wss, "wss://") {
		scheme = "https"
	}
	if i := strings.Index(u, "/"); i >= 0 {
		u = u[:i]
	}
	return scheme + "://" + u
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadDotEnv 解析 KEY=VALUE 格式的 .env 文件，已存在的环境变量不覆盖。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // 没有 .env 不算错误
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}
