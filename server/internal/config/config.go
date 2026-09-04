// 配置加载：环境变量优先，附带简易 .env 解析（不覆盖已有环境变量）。
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Config struct {
	Addr        string // HTTP 监听地址
	DataDir     string // 数据目录：数据库、证书、日志等持久化边界
	DBPath      string // sqlite 文件路径（DATABASE_URL 为空时使用）
	DatabaseURL string // 数据库连接串：空/sqlite = sqlite，mysql:// 或 postgres:// 切换后端
	CORSOrigin  string // 允许的跨域来源，* 表示全部
	StaticDir   string // 可选：前端构建产物目录，空则不托管
	RegOpen     bool   // 兼容旧变量 REGISTRATION_OPEN=true（等价 REG_POLICY=open）
	RegPolicy   string // 注册策略默认值：closed / invite / open（可被后台设置覆盖）
	PublicURL   string // 站点公开地址（拼邀请链接用）；空 = 按请求推导
	SiteName    string // 站点名（/api/site 下发给前端展示）
}

func Load() Config {
	loadDotEnv(".env")
	dataDir := resolveDataDir()
	// 显式指定的 --data/HEARTH_DATA 可能还不存在，先建好（失败由后续 DB 打开报错）
	os.MkdirAll(dataDir, 0o700)
	os.Chmod(dataDir, 0o700)
	// 数据目录里的 .env 再读一次：单文件分发时工作目录不确定，配置跟数据走。
	// loadDotEnv 不覆盖已有值，工作目录的 .env 优先。
	loadDotEnv(filepath.Join(dataDir, ".env"))
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "hearth.db")
		// 只在没有显式数据目录时兼容旧版工作目录数据库；否则会悄悄绕过
		// --data / HEARTH_DATA 指定的持久化边界。
		if dataFlag() == "" && os.Getenv("HEARTH_DATA") == "" {
			if _, err := os.Stat("hearth.db"); err == nil {
				dbPath = "hearth.db"
			}
		}
	}
	return Config{
		Addr:        env("ADDR", ":8080"),
		DataDir:     dataDir,
		DBPath:      dbPath,
		DatabaseURL: env("DATABASE_URL", ""),
		CORSOrigin:  env("CORS_ORIGIN", "*"),
		StaticDir:   env("STATIC_DIR", ""),
		RegOpen:     env("REGISTRATION_OPEN", "") == "true",
		RegPolicy:   env("REG_POLICY", ""),
		PublicURL:   env("PUBLIC_URL", ""),
		SiteName:    env("SITE_NAME", "Hearth"),
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

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolveDataDir 数据目录：--data / HEARTH_DATA 指定；否则优先可执行文件旁的 data/
// （便携优先，与容器 /data 同语义），写不进去（Program Files 之类）再回落用户目录。
func resolveDataDir() string {
	if v := dataFlag(); v != "" {
		return v
	}
	if v := os.Getenv("HEARTH_DATA"); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		if p, err := filepath.EvalSymlinks(exe); err == nil {
			exe = p
		}
		if dir := filepath.Join(filepath.Dir(exe), "data"); writableDir(dir) {
			return dir
		}
	}
	return fallbackDataDir()
}

// dataFlag 手扫 --data：config.Load 跑在 flag 解析之前，子命令形态也用不上 flag 包。
func dataFlag() string {
	args := os.Args[1:]
	for i, a := range args {
		if (a == "--data" || a == "-data") && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--data="); ok {
			return v
		}
		if v, ok := strings.CutPrefix(a, "-data="); ok {
			return v
		}
	}
	return ""
}

func writableDir(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	_ = os.Chmod(dir, 0o700)
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func fallbackDataDir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "Hearth")
		}
		return filepath.Join(home, "AppData", "Local", "Hearth")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Hearth")
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "hearth")
		}
		return filepath.Join(home, ".local", "share", "hearth")
	}
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
