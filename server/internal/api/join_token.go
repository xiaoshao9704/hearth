// 进房令牌：签发语音/舞台凭证与同源信令补全。
package api

import (
	"errors"
	"log"
	"net/http"
	neturl "net/url"
	"strings"

	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

// ---- 进房令牌 ----

func (a *API) joinToken(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Channel   string `json:"channel"`
		ChannelID int64  `json:"channel_id"` // 频道寻址往 id 靠：>0 时优先，名字继续接受
		DeviceID  string `json:"device_id"`  // 前端 localStorage 持久化的设备 ID
	}
	if !decode(w, r, &req) {
		return
	}
	if req.ChannelID <= 0 && req.Channel == "" {
		writeErr(w, http.StatusBadRequest, "缺少频道")
		return
	}
	var c *store.Channel
	var err error
	if req.ChannelID > 0 {
		c, err = a.st.ChannelByID(r.Context(), req.ChannelID)
	} else {
		c, err = a.channelByRef(r.Context(), req.Channel)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "频道不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	// 入场判定：封禁/邀请制决定能否进入，禁言随进房凭证生效（无发布权限）
	adm, ok, reason, err := a.admitUser(r.Context(), c, u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if !ok {
		writeErr(w, http.StatusForbidden, reason)
		return
	}
	// 参与者元数据组一次两线共用：身份判定走 identity 里的 user_id，
	// 用户名等展示信息随元数据下发（前端不解析 identity）
	meta := rtc.Meta{UID: adm.UID, Username: adm.Username, Tag: a.deviceTagFor(r, req.DeviceID, u.ID)}
	// 频道名即内核房间名，一一映射。语音线必发；舞台线可选；
	// 两线同一实例时标记 combined，前端用一条连接承担两种角色（即旧单线形态）。
	voiceAlias, voiceP := a.voiceInstance(r.Context())
	stageAlias, stageP := a.stageInstance(r.Context())
	vc, err := voiceP.JoinCredentials(r.Context(), c.Name, meta, adm.CanPublish)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "签发令牌失败")
		return
	}
	resp := map[string]any{"voice": a.fillCred(r, vc, voiceAlias)}
	combined := stageP != nil && voiceAlias == stageAlias
	resp["combined"] = combined
	// 聊天与文件走哪条线的数据通道由服务端决定并下发，前端不自己猜拓扑
	resp["data_line"] = a.dynVal(r.Context(), "chat_data_line")
	if stageP != nil && !combined {
		sc, serr := stageP.JoinCredentials(r.Context(), c.Name, meta, adm.CanPublish)
		if serr != nil {
			// 仍 200 发语音票（语音可用），但带 stage_error 让客户端区分「没配舞台」与「签发失败」
			log.Printf("舞台线签发失败（语音照常）: %v", serr)
			resp["stage_error"] = "签发令牌失败"
		} else {
			resp["stage"] = a.fillCred(r, sc, stageAlias)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// fillCred 补全内核未声明的连接信息：走同源 /providers/{alias} 信令代理。
func (a *API) fillCred(r *http.Request, c rtc.Credentials, alias string) map[string]string {
	u := c.URL
	if u == "" {
		u = a.signalURL(r, alias)
	}
	token := c.Token
	if token == "" {
		token = BearerToken(r)
	}
	return map[string]string{"engine": c.Engine, "url": u, "token": token}
}

// deviceTagFor 设备标签 = UA 推断 + 前端持久设备 ID（缺省时随机，不建档）；
// 语音线与舞台线共用，保证同一设备两线 identity 一致。
func (a *API) deviceTagFor(r *http.Request, deviceID string, userID int64) string {
	tag := deviceTag(r.UserAgent())
	if dev := deviceIDRe.FindString(deviceID); dev != "" {
		if err := a.st.RecordDevice(r.Context(), userID, dev, tag); err != nil {
			log.Printf("记录设备失败: %v", err)
		}
		return tag + "-" + dev
	}
	return tag + "-" + randHex(2)
}

// signalURL 按请求推导同源信令代理地址（/providers/{alias} 路由）。
func (a *API) signalURL(r *http.Request, alias string) string {
	scheme := "ws"
	if requestScheme(r) == "https" {
		scheme = "wss"
	}
	return (&neturl.URL{Scheme: scheme, Host: r.Host, Path: "/providers/" + alias}).String()
}

// deviceTag 从 User-Agent 提取简短设备标签(如 mac/iphone/win-edge),区分同账号多设备。
func deviceTag(ua string) string {
	ua = strings.ToLower(ua)
	tag := "web"
	switch {
	case strings.Contains(ua, "iphone"):
		tag = "iphone"
	case strings.Contains(ua, "ipad"):
		tag = "ipad"
	case strings.Contains(ua, "android"):
		tag = "android"
	case strings.Contains(ua, "windows"):
		tag = "win"
	case strings.Contains(ua, "mac os"), strings.Contains(ua, "macintosh"):
		tag = "mac"
	case strings.Contains(ua, "linux"):
		tag = "linux"
	}
	// 非主流浏览器补充标注(Chrome 省略)
	switch {
	case strings.Contains(ua, "edg/"):
		tag += "-edge"
	case strings.Contains(ua, "firefox/"):
		tag += "-ff"
	case !strings.Contains(ua, "chrome/") && strings.Contains(ua, "safari/"):
		tag += "-safari"
	}
	return tag
}

