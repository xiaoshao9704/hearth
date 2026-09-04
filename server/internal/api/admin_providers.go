// 管理后台的内核实例 CRUD：DB 注册实例的增删改查；内建与 env 锁定实例只读。
// 每次增删改成功后 reloadProviders 重建注册表（未变化实例复用旧对象，见 providers.go）。
package api

import (
	"errors"
	"net/http"
	"strings"

	"hearth/server/internal/rtc"
	"hearth/server/internal/store"

	"github.com/go-chi/chi/v5"
)

// 可注册类型（内建 bellows/lkembed 不在列）；Label 供管理后台渲染。
var registrableTypes = []struct {
	Type  string
	Label string
}{
	{TypeLivekit, "LiveKit"},
	{TypeLivekitIngress, "LiveKit Ingress"},
	{TypeBellowsRemote, "远端 Bellows"},
}

// providerView 实例的管理后台视图：Secret 字段值掩码为空串，params_set 报告是否已设置。
// nil 入参返回空视图（reload 失败等异常路径不 panic）。
func (a *API) providerView(inst *ProviderInstance) map[string]any {
	if inst == nil {
		return map[string]any{"alias": "", "type": "", "caps": []string{},
			"params": map[string]string{}, "params_set": map[string]bool{}}
	}
	params := map[string]string{}
	for k, v := range inst.Params {
		params[k] = v
	}
	paramsSet := map[string]bool{}
	for _, f := range a.providerTypeFields(inst.Type) {
		if f.Secret {
			paramsSet[f.Name] = inst.Params[f.Name] != ""
			params[f.Name] = ""
		}
	}
	caps := inst.Caps()
	if caps == nil {
		caps = []string{}
	}
	return map[string]any{
		"alias": inst.Alias, "type": inst.Type, "caps": caps,
		"locked": inst.Locked, "builtin": inst.Builtin,
		"params": params, "params_set": paramsSet,
	}
}

// adminListProviders 全部实例 + 可注册类型的字段模式（注册表单据此渲染）。
func (a *API) adminListProviders(w http.ResponseWriter, r *http.Request) {
	instances := []map[string]any{}
	for _, inst := range a.listInstances(r.Context()) {
		instances = append(instances, a.providerView(inst))
	}
	types := []map[string]any{}
	for _, t := range registrableTypes {
		types = append(types, map[string]any{
			"type": t.Type, "label": t.Label, "fields": a.providerTypeFields(t.Type),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": instances, "types": types})
}

// checkProviderParams 按字段模式校验并归一化 params：只保留声明字段、值去空白。
// 非 Secret 字段必填非空（livekit 的 livekit_url 可选，空串即清除）；
// Secret 字段：POST（old 为 nil）必填，PUT 空串 = 保留旧值。
func checkProviderParams(typ string, fields []rtc.ConfigKey, in, old map[string]string) (map[string]string, string) {
	out := map[string]string{}
	for _, f := range fields {
		v := strings.TrimSpace(in[f.Name])
		if f.Secret {
			if v == "" && old != nil {
				v = old[f.Name]
			}
			if v == "" {
				return nil, f.Label + " 必填"
			}
		} else if v == "" && !(typ == TypeLivekit && f.Name == "livekit_url") {
			return nil, f.Label + " 必填"
		}
		if v != "" {
			out[f.Name] = v
		}
	}
	return out, ""
}

func (a *API) adminCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type   string            `json:"type"`
		Alias  string            `json:"alias"`
		Params map[string]string `json:"params"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Alias = strings.TrimSpace(req.Alias)
	if !aliasRe.MatchString(req.Alias) {
		writeErr(w, http.StatusBadRequest, "alias 仅限小写字母数字与 -，1-32 位")
		return
	}
	fields := a.providerTypeFields(req.Type)
	if fields == nil {
		writeErr(w, http.StatusBadRequest, "未知实例类型: "+req.Type)
		return
	}
	if reservedAliases[req.Alias] || a.instance(req.Alias) != nil {
		writeErr(w, http.StatusConflict, "alias 已被占用（含内建与 env 锁定实例）")
		return
	}
	params, msg := checkProviderParams(req.Type, fields, req.Params, nil)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	rec := &store.ProviderRecord{Alias: req.Alias, Type: req.Type, Params: params}
	err := a.mutateProviders(r.Context(), func() error {
		return a.st.CreateProvider(r.Context(), rec)
	})
	if store.IsUniqueViolation(err) {
		writeErr(w, http.StatusConflict, "alias 已被占用（含内建与 env 锁定实例）")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	inst := a.instance(req.Alias)
	if inst == nil {
		writeErr(w, http.StatusInternalServerError, "实例加载失败")
		return
	}
	writeJSON(w, http.StatusCreated, a.providerView(inst))
}

func (a *API) adminUpdateProvider(w http.ResponseWriter, r *http.Request) {
	alias := chi.URLParam(r, "alias")
	inst := a.instance(alias)
	if inst == nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	if msg := readonlyMsg(inst); msg != "" {
		writeErr(w, http.StatusConflict, msg)
		return
	}
	var req struct {
		Params map[string]string `json:"params"`
	}
	if !decode(w, r, &req) {
		return
	}
	old, err := a.st.ProviderByAlias(r.Context(), alias)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	params, msg := checkProviderParams(inst.Type, a.providerTypeFields(inst.Type), req.Params, old.Params)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	err = a.mutateProviders(r.Context(), func() error {
		return a.st.UpdateProviderParams(r.Context(), alias, params)
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) adminDeleteProvider(w http.ResponseWriter, r *http.Request) {
	alias := chi.URLParam(r, "alias")
	inst := a.instance(alias)
	if inst == nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	if msg := readonlyMsg(inst); msg != "" {
		writeErr(w, http.StatusConflict, msg)
		return
	}
	// 引用保护：选择器当前生效值指向该实例时先拒绝，避免删出回落实例的隐式行为变化
	for _, sel := range selectorKeys {
		if a.dynVal(r.Context(), sel.Name) == alias {
			writeErr(w, http.StatusConflict, sel.Label+" 正引用该实例，先切换选择器再删除")
			return
		}
	}
	if err := a.mutateProviders(r.Context(), func() error {
		return a.st.DeleteProvider(r.Context(), alias)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "内部错误")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readonlyMsg 内建与 env 锁定实例只读；可写返回空串。
func readonlyMsg(inst *ProviderInstance) string {
	if inst.Builtin {
		return "内建实例只读"
	}
	if inst.Locked {
		return "该实例由环境变量锁定，改部署侧配置后重启生效"
	}
	return ""
}
