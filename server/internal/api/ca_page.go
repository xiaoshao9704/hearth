// 根证书安装说明页（GET /ca）：无鉴权的服务端渲染静态页，不进 SPA 路由。
// 面向的是「拿到链接、还没信任这台服务器」的人，所以不依赖前端资源、不依赖 https。
package api

import (
	"html/template"
	"net"
	"net/http"
	"strings"
)

type caPageData struct {
	Enabled     bool // 来源是自签且根证书已生成
	Source      string
	Fingerprint string
	Merged      bool
	HTTPSPort   string
	URLs        []string // 当前证书 SAN 里可以直接打开的 https 地址
}

// caPage 根证书安装说明页。
func (a *API) caPage(w http.ResponseWriter, r *http.Request) {
	st := a.tlsStore.Status()
	d := caPageData{
		Source:    st.Source,
		Merged:    !a.cfg.TLSSplit(),
		HTTPSPort: a.httpsPort(),
	}
	if st.Source == "self" && st.CA != nil {
		d.Enabled = true
		d.Fingerprint = st.CA.FingerprintSHA256
	}
	if st.Cert != nil {
		for _, s := range st.Cert.SANs {
			host := s
			if ip := net.ParseIP(s); ip != nil && ip.To4() == nil {
				host = "[" + s + "]" // IPv6 字面量进 URL 要加方括号
			}
			d.URLs = append(d.URLs, "https://"+joinHostPort(host, d.HTTPSPort))
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	caTmpl.Execute(w, d)
}

// net.JoinHostPort 会给已带方括号的 v6 再加一层，这里自己拼。
func joinHostPort(host, port string) string {
	if strings.HasPrefix(host, "[") {
		return host + ":" + port
	}
	return net.JoinHostPort(host, port)
}

var caTmpl = template.Must(template.New("ca").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>安装 Hearth 根证书</title>
<style>
:root { color-scheme: light dark; }
body { margin: 0 auto; padding: 24px 18px 64px; max-width: 44rem; line-height: 1.75;
  font: 16px/1.75 -apple-system, BlinkMacSystemFont, "Segoe UI", "Noto Sans SC", sans-serif; }
h1 { font-size: 1.5rem; margin: 0 0 .5rem; }
h2 { font-size: 1.1rem; margin: 2rem 0 .5rem; }
h3 { font-size: 1rem; margin: 1.2rem 0 .3rem; }
code, .fp { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .85em;
  word-break: break-all; }
.fp { display: block; padding: .6rem .8rem; border: 1px solid rgba(128,128,128,.4); border-radius: 8px; }
.btn { display: inline-block; margin: .8rem 0; padding: .6rem 1.2rem; border-radius: 8px;
  background: #d1502a; color: #fff; text-decoration: none; font-weight: 600; }
.warn { padding: .8rem 1rem; border-left: 3px solid #d1502a; background: rgba(209,80,42,.08); }
ol, ul { padding-left: 1.4rem; }
li { margin: .25rem 0; }
details { margin-top: 2rem; }
summary { cursor: pointer; font-weight: 600; }
.muted { opacity: .75; font-size: .9rem; }
</style></head><body>
{{if not .Enabled}}
<h1>无需安装证书</h1>
{{if eq .Source "off"}}
<p>这台 Hearth 没有开启自带的 https（证书来源 <code>off</code>）。如果你是用 https 地址访问的，
那是前面的反向代理提供的证书，浏览器本来就信任，不需要装任何东西。</p>
{{else}}
<p>这台 Hearth 用的是外部证书（证书来源 <code>{{.Source}}</code>），浏览器本来就信任它，
不需要安装任何东西。直接用站长给你的 https 地址打开即可。</p>
{{end}}
<p class="muted">如果浏览器仍然提示不安全，请联系站长确认证书配置。</p>
</body></html>
{{else}}
<h1>安装 Hearth 根证书</h1>

<h2>1. 这是什么</h2>
<p>这是这台 Hearth 自己生成的根证书。装上它，你的浏览器才会信任这台服务器的 https 地址，
麦克风、投屏、通知才能用——浏览器只在「安全上下文」下才给这些权限。</p>
<p>这份根证书的 SHA-256 指纹：</p>
<code class="fp">{{.Fingerprint}}</code>
<p class="warn">装之前请和站长核对这串指纹，<strong>不一致就不要装</strong>。</p>
<p><a class="btn" href="/ca.crt">下载根证书</a></p>

<h2>2. 它能做什么、不能做什么</h2>
<ul>
<li>能：让这台 Hearth 的地址在你的设备上显示为安全连接。</li>
<li>不能：冒充任何域名网站。这份根证书带「名称约束」，只允许为 <code>localhost</code>、
站长登记的主机名和 IP 地址签发证书；用它签任何别的域名，浏览器都会直接拒绝。</li>
<li>私钥只在服务器上，网页上下载到的是公开的证书文件，泄漏了也签不出东西。</li>
</ul>

<h2>3. 安装步骤</h2>

<h3>macOS</h3>
<ol>
<li>点上面的「下载根证书」，双击下载到的 <code>hearth-ca.crt</code>。</li>
<li>系统会打开「钥匙串访问」，在「登录」钥匙串里找到名为 <strong>Hearth CA</strong> 的一项。</li>
<li>双击它（或右键 → 显示简介），展开「信任」。</li>
<li>把「使用此证书时」改成「<strong>始终信任</strong>」。</li>
<li>关闭窗口，按提示输入开机密码确认。</li>
</ol>

<h3>Windows</h3>
<ol>
<li>下载后双击 <code>hearth-ca.crt</code>，点「<strong>安装证书</strong>」。</li>
<li>存储位置选「<strong>当前用户</strong>」，下一步。</li>
<li>选「<strong>将所有证书都放入下列存储</strong>」，点「浏览」，选「<strong>受信任的根证书颁发机构</strong>」。</li>
<li>下一步 → 完成。会弹出「<strong>安全警告</strong>」问你是否安装，选「<strong>是</strong>」。</li>
</ol>

<h3>Android</h3>
<ol>
<li>用浏览器下载根证书文件。</li>
<li>打开 设置 → 安全（或「加密与凭据」）→ <strong>安装证书</strong> → <strong>CA 证书</strong>。</li>
<li>系统会提示「<strong>你的数据将不再是私密的</strong>」——这是所有用户安装 CA 时的固定提示，
选「<strong>仍然安装</strong>」。</li>
<li>在文件列表里选中刚下载的 <code>hearth-ca.crt</code>。</li>
</ol>

<h3>iOS / iPadOS</h3>
<ol>
<li><strong>用 Safari</strong> 打开本页并下载证书；系统提示「此网站正尝试下载一个配置描述文件」，
选「<strong>允许</strong>」。</li>
<li>打开 设置，最顶部会出现「<strong>已下载描述文件</strong>」，点进去 → 右上角「安装」。</li>
<li>输入锁屏密码，再点一次「安装」。</li>
<li><strong>还没完</strong>：去 设置 → 通用 → 关于本机 → <strong>证书信任设置</strong>，
打开「Hearth CA」的完全信任开关。不做这一步 Safari 仍然报不安全。</li>
</ol>

<h2>4. 装完怎么打开</h2>
{{if .Merged}}
<p>地址<strong>前面要带 https</strong>——和平时用的是同一个端口，只是协议不同，
少打一个 s 就是不安全连接，麦克风权限点不开。</p>
{{else}}
<p>https 用的是单独的端口 <code>{{.HTTPSPort}}</code>，别用明文那个端口。</p>
{{end}}
{{if .URLs}}
<p>这台 Hearth 的证书涵盖以下地址（挑你网络里能通的那个）：</p>
<ul>{{range .URLs}}<li><code>{{.}}</code></li>{{end}}</ul>
{{end}}

<h2>5. 不想要了怎么卸载</h2>
<ul>
<li>macOS：钥匙串访问 → 找到「Hearth CA」→ 删除。</li>
<li>Windows：运行 <code>certmgr.msc</code> → 受信任的根证书颁发机构 → 证书 → 找到「Hearth CA」→ 删除。</li>
<li>Android：设置 → 安全 → 加密与凭据 → 清除凭据（或在「用户凭据」里单独删除）。</li>
<li>iOS / iPadOS：设置 → 通用 → VPN 与设备管理 → 移除该描述文件。</li>
</ul>

<details>
<summary>站长须知</summary>
<p>根证书的私钥在服务器的数据目录里（<code>tls/ca.key</code>），不经任何接口读出。
备份泄漏或需要放宽名称约束时，到管理后台的「TLS 与对外地址」点「重新生成根证书」——
之后所有装过旧根的设备都要重装。</p>
<p>有域名的话不必让别人装根证书：用外部工具（acme.sh / lego / certbot / Caddy 等）签一张证书，
把证书来源改成 <code>file</code> 或 <code>upload</code> 即可。</p>
</details>
</body></html>
{{end}}
`))
