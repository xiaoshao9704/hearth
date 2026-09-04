// Package ddns 公网地址变化时自动更新域名解析（DDNS）。
//
// 提供方实现 Provider 接口：Update 把主机名的 A/AAAA 记录指向给定地址，
// 零值地址表示不更新该记录（只更新另一种）。内置 DuckDNS / Cloudflare /
// DNSPod（腾讯云）/ 阿里云，按「零成本先行」排序；dyndns2 通用协议在计划第四阶段。
//
// Runner 参照 tlsx.Manager 的 Sync 模式：调用方按宣告探测的节拍（RefreshAnnounce
// 同节拍：端口映射变化回调 + 周期任务 + 配置保存）把当前配置与公网地址快照喂进来，
// Runner 自己去重——地址集合没变不打 API；上次推送成功的目标落 <data>/ddns-state.json，
// 重启后不重复打；失败指数退避（1min 起步封顶 10min）。
package ddns

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/netip"
	"os"
	"sync"
	"time"
)

// Provider 一个 DDNS 提供方。
type Provider interface {
	Name() string
	// Update 把 host 的 A（v4）/AAAA（v6）记录更新为给定地址；任一零值 = 不更新该记录。
	Update(ctx context.Context, host string, v4, v6 netip.Addr) error
}

// Config 一次 Sync 看到的 DDNS 相关动态配置快照（凭证已解析进各字段）。
type Config struct {
	Provider string // off / duckdns / cloudflare / dnspod / aliyun
	Host     string // 要更新的主机名（FQDN）

	DuckDNSToken string
	CFToken      string // Cloudflare API Token
	DNSPodID     string
	DNSPodToken  string
	AliyunID     string
	AliyunSecret string
}

// Status 运行状态，进管理后台概览与网络自检回显。
type Status struct {
	Provider  string    `json:"provider"`            // 当前选择的提供方（off = 未启用）
	Host      string    `json:"host"`                // 目标主机名
	V4        string    `json:"v4,omitempty"`        // 上次成功推送的 A 记录值
	V6        string    `json:"v6,omitempty"`        // 上次成功推送的 AAAA 记录值
	UpdatedAt time.Time `json:"updated_at"`          // 上次成功推送时间（未知为零值）
	LastError string    `json:"last_error"`          // 上次推送错误（空 = 正常）
	NextRetry time.Time `json:"next_retry,omitzero"` // 失败退避中的下次重试时间
}

// New 按配置构造提供方；off/未知/凭证不全返回 nil（视为未启用，不算错误——
// 凭证可能在向导里还没填完，状态回显会说明缺什么）。
func New(cfg Config) Provider {
	switch cfg.Provider {
	case "duckdns":
		if cfg.DuckDNSToken == "" {
			return nil
		}
		return &DuckDNS{Token: cfg.DuckDNSToken}
	case "cloudflare":
		if cfg.CFToken == "" {
			return nil
		}
		return &Cloudflare{Token: cfg.CFToken}
	case "dnspod":
		if cfg.DNSPodID == "" || cfg.DNSPodToken == "" {
			return nil
		}
		return &DNSPod{ID: cfg.DNSPodID, Token: cfg.DNSPodToken}
	case "aliyun":
		if cfg.AliyunID == "" || cfg.AliyunSecret == "" {
			return nil
		}
		return &Aliyun{ID: cfg.AliyunID, Secret: cfg.AliyunSecret}
	}
	return nil
}

// 退避参数：1min 起步、翻倍、封顶 10min；成功后复位。
const (
	retryStart = time.Minute
	retryMax   = 10 * time.Minute
)

// storedState 落盘的上次成功推送目标（<data>/ddns-state.json）：重启后地址没变就不重复打 API。
type storedState struct {
	Provider  string    `json:"provider"`
	Host      string    `json:"host"`
	V4        string    `json:"v4"`
	V6        string    `json:"v6"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Runner 持有提供方配置与推送状态。全部导出方法并发安全。
type Runner struct {
	statePath string
	prov      Provider // 测试注入用；nil = 按 Config 现构造

	mu        sync.Mutex
	status    Status
	pushed    storedState // 上次成功推送的目标（进程内权威，落盘是它的副本）
	hasPushed bool
	nextRetry time.Time
	retryWait time.Duration
}

// NewRunner statePath 是推送状态的落盘路径（<data>/ddns-state.json）。
func NewRunner(statePath string) *Runner {
	r := &Runner{statePath: statePath, retryWait: retryStart}
	if b, err := os.ReadFile(statePath); err == nil {
		var st storedState
		if json.Unmarshal(b, &st) == nil && st.Host != "" {
			r.pushed = st
			r.hasPushed = true
		}
	}
	return r
}

// Status 当前状态快照。
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Sync 按当前配置与公网地址快照 reconcile：off/凭证不全/缺主机名时不动作；
// 地址集合与上次成功推送一致时不动作；否则调提供方 Update，失败按退避计时重试。
// 幂等，与 RefreshAnnounce 同节拍被调。
func (r *Runner) Sync(ctx context.Context, cfg Config, externals []string) {
	v4, v6 := splitExternals(externals)

	r.mu.Lock()
	r.status.Provider = cfg.Provider
	r.status.Host = cfg.Host
	r.mu.Unlock()

	if cfg.Provider == "" || cfg.Provider == "off" {
		r.mu.Lock()
		r.status = Status{Provider: "off"}
		r.mu.Unlock()
		return
	}
	p := r.prov
	if p == nil {
		p = New(cfg)
	}
	if p == nil {
		r.setError("凭证未填全，请在管理后台补齐该提供方的密钥")
		return
	}
	if cfg.Host == "" {
		r.setError("未配置要更新的主机名（ddns_host）")
		return
	}
	if !v4.IsValid() && !v6.IsValid() {
		r.setError("还没有探测到本机公网地址（STUN 不可达且未建端口映射）")
		return
	}

	target := storedState{Provider: cfg.Provider, Host: cfg.Host, UpdatedAt: r.pushed.UpdatedAt}
	if v4.IsValid() {
		target.V4 = v4.String()
	}
	if v6.IsValid() {
		target.V6 = v6.String()
	}

	r.mu.Lock()
	unchanged := r.hasPushed && r.status.LastError == "" &&
		r.pushed.Provider == target.Provider && r.pushed.Host == target.Host &&
		r.pushed.V4 == target.V4 && r.pushed.V6 == target.V6
	backoff := !r.nextRetry.IsZero() && time.Now().Before(r.nextRetry)
	r.mu.Unlock()
	if unchanged || backoff {
		return
	}

	if err := p.Update(ctx, cfg.Host, v4, v6); err != nil {
		log.Printf("DDNS 更新失败（%s）: %v", cfg.Provider, err)
		r.mu.Lock()
		r.status.LastError = err.Error()
		r.status.NextRetry = time.Now().Add(r.retryWait)
		r.nextRetry = r.status.NextRetry
		r.retryWait = min(r.retryWait*2, retryMax)
		r.mu.Unlock()
		return
	}
	log.Printf("DDNS 已更新 %s → %s", cfg.Host, recordsText(v4, v6))
	target.UpdatedAt = time.Now()
	r.mu.Lock()
	r.pushed = target
	r.hasPushed = true
	r.status = Status{Provider: cfg.Provider, Host: cfg.Host, V4: target.V4, V6: target.V6, UpdatedAt: target.UpdatedAt}
	r.nextRetry = time.Time{}
	r.retryWait = retryStart
	r.mu.Unlock()
	r.persist(target)
}

func (r *Runner) setError(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.LastError = msg
	r.status.NextRetry = time.Time{}
}

// persist 把上次成功推送目标落盘（失败只打日志，状态还在内存里）。
func (r *Runner) persist(st storedState) {
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.WriteFile(r.statePath, b, 0o600); err != nil {
		log.Printf("DDNS 状态落盘失败: %v", err)
	}
}

// splitExternals 宣告探测快照（可能带端口）拆出第一个公网 v4 / v6 地址。
func splitExternals(externals []string) (v4, v6 netip.Addr) {
	for _, e := range externals {
		s := e
		if ap, err := netip.ParseAddrPort(e); err == nil { // 映射结果带端口
			s = ap.Addr().String()
		}
		ip, err := netip.ParseAddr(s)
		if err != nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			continue
		}
		if ip.Is4() && !v4.IsValid() {
			v4 = ip
		}
		if ip.Is6() && !v6.IsValid() {
			v6 = ip
		}
	}
	return v4, v6
}

func recordsText(v4, v6 netip.Addr) string {
	s := ""
	if v4.IsValid() {
		s += "A=" + v4.String()
	}
	if v6.IsValid() {
		if s != "" {
			s += " "
		}
		s += "AAAA=" + v6.String()
	}
	return s
}

// apiErr 统一组提供方错误文案（状态回显用，中文）。
func apiErr(provider, detail string) error {
	return errors.New(provider + ": " + detail)
}
