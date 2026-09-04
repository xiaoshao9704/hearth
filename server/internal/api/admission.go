// 入场判定统一：谁能进房、能否发布（封禁/邀请制/禁言）收敛到本文件，
// joinToken（凭证签发）与 WHIP 推流（/w POST 的 admitIngest）两个入口共用同一套判定。
package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

// admission 入场判定结果。身份一律以 user_id 为准（identity 由 rtc.Identity 组），
// 用户名只作为展示信息随参与者元数据下发。
type admission struct {
	UID        int64  // 参与者归属用户 id
	Username   string // 展示名
	CanPublish bool   // 能否发布（未被禁言）
}

// admitUser 统一入场判定：封禁/邀请制决定能否进入，禁言决定能否发布。
// 返回 (判定结果, 是否允许进入, 拒绝原因, 内部错误)。
func (a *API) admitUser(ctx context.Context, c *store.Channel, u *store.User) (admission, bool, string, error) {
	ok, reason, err := a.st.CanJoin(ctx, c, u.ID)
	if err != nil {
		return admission{}, false, "", err
	}
	if !ok {
		return admission{}, false, reason, nil
	}
	gagged, err := a.st.IsGagged(ctx, c.ID, u.ID)
	if err != nil {
		return admission{}, false, "", err
	}
	return admission{UID: u.ID, Username: u.Username, CanPublish: !gagged}, true, "", nil
}

// ---- 推流入场判定（/w POST）----

// ingestCtxKey 推流判定结果的传递：admitIngest 在 serveWHIP 做完后把组好的
// 身份四元组挂到请求 ctx，whipResolver（ResolveFunc 只有令牌参数，频道在 URL 里
// 由接入层解析）原样取回。
type ingestCtxKey struct{}

// ingestAdmission 推流入场判定结果：Room=频道名（=内核房间名），Identity 见 rtc.Identity，
// Meta 是随参与者下发的元数据（前端据 meta.uid 认人、meta.username 展示）。
type ingestAdmission struct {
	Room     string
	Identity string
	Meta     rtc.Meta
}

// admitIngest 统一推流入场判定（替代旧 canPublishByStreamKey）。
// 推流不再是独立选择器：URL 的 alias 段必须是当前舞台实例（stage_provider 选中的那个，
// 推进别的实例观众看不到），且该实例有 WHIP 推流能力，否则 definitive 404——
// 已退场的旧内建推流实例等历史形态因此天然 404。
// 其后：令牌反查用户 + URL 取频道 → admitUser（封禁/邀请制/禁言）。全部 definitive：
// 令牌不存在 404、频道不存在 404、不许推（封禁/邀请制/禁言/账号停用）403、查询出错 503——
// 不再有 fail-open：上游收到的已是 hearth 出示的实例凭证，不再承担鉴权。
// 返回 false 时响应已写好。
func (a *API) admitIngest(ctx context.Context, w http.ResponseWriter, alias, channel, token string) (ingestAdmission, bool) {
	stageAlias, sp := a.stageInstance(ctx)
	inst := a.instance(alias)
	if sp == nil || alias != stageAlias || inst == nil || inst.Ingest == nil {
		writeErr(w, http.StatusNotFound, "推流入口不存在（OBS 请指向当前舞台内核实例的地址）")
		return ingestAdmission{}, false
	}
	if token == "" {
		writeErr(w, http.StatusBadRequest, "缺少推流令牌")
		return ingestAdmission{}, false
	}
	fail := func(err error) (ingestAdmission, bool) {
		log.Printf("推流入场判定查询失败（实例 %s）: %v", alias, err)
		writeErr(w, http.StatusServiceUnavailable, "推流入口暂时不可用，请稍后再试")
		return ingestAdmission{}, false
	}
	it, err := a.st.IngestTokenByToken(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "推流令牌无效或已重置")
		return ingestAdmission{}, false
	}
	if err != nil {
		return fail(err)
	}
	u, err := a.st.UserByID(ctx, it.UserID)
	if errors.Is(err, store.ErrNotFound) { // 用户已删除，令牌等同失效
		writeErr(w, http.StatusNotFound, "推流令牌无效或已重置")
		return ingestAdmission{}, false
	}
	if err != nil {
		return fail(err)
	}
	c, err := a.st.ChannelByName(ctx, channel)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "频道不存在")
		return ingestAdmission{}, false
	}
	if err != nil {
		return fail(err)
	}
	if u.Disabled {
		writeErr(w, http.StatusForbidden, "账号已被停用")
		return ingestAdmission{}, false
	}
	adm, ok, reason, err := a.admitUser(ctx, c, u)
	if err != nil {
		return fail(err)
	}
	if !ok {
		writeErr(w, http.StatusForbidden, reason)
		return ingestAdmission{}, false
	}
	if !adm.CanPublish {
		writeErr(w, http.StatusForbidden, "你已被禁言，无法推流")
		return ingestAdmission{}, false
	}
	return ingestAdmission{
		Room:     c.Name,
		Identity: rtc.Identity(u.ID, it.Tag),
		Meta:     rtc.Meta{UID: u.ID, Username: u.Username, Kind: "ingest", Tag: it.Tag},
	}, true
}
