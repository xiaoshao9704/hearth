// 版本回显与新版本提示：/api/admin/version 返回当前版本；update_check 开着时
// 查一次 GitHub Releases 的 latest（3s 超时、进程内缓存 1 小时、失败静默记 detail）。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// releaseRepo 发布仓库（GitHub Releases 的唯一出处；只在这里出现一次）。
const releaseRepo = "xiaoshao9704/hearth"

// SetVersion 注入当前版本号（main 的 -X main.version；默认 dev）。
func (a *API) SetVersion(v string) { a.version = v }

type versionInfo struct {
	Version  string `json:"version"`          // 当前版本（dev = 未走发布流水线）
	Latest   string `json:"latest,omitempty"` // 远端最新 release tag
	Outdated bool   `json:"outdated"`         // 有更新
	URL      string `json:"url,omitempty"`    // 发布页地址（有新版本时给；仓库名只有 releaseRepo 一处出处）
	Detail   string `json:"detail,omitempty"` // 检查关闭或失败原因（静默，只回显）
}

var (
	releaseCacheMu sync.Mutex
	releaseCache   versionInfo
	releaseCacheAt time.Time
)

// adminVersion GET /api/admin/version。
func (a *API) adminVersion(w http.ResponseWriter, r *http.Request) {
	cur := a.version
	if cur == "" {
		cur = "dev"
	}
	info := versionInfo{Version: cur}
	if a.dynVal(r.Context(), "update_check") == "off" {
		info.Detail = "新版本检查已关闭（update_check=off）"
		writeJSON(w, http.StatusOK, info)
		return
	}
	latest, detail := a.latestRelease(r.Context())
	info.Latest = latest
	info.Detail = detail
	if latest != "" && cur != "dev" {
		// tag 形如 v0.4.0；剥离 v 前缀做字符串相等判断（严格 semver 比较不值得引依赖，
		// 发布序就是 release 序，不相等即提示）
		if strings.TrimPrefix(latest, "v") != strings.TrimPrefix(cur, "v") {
			info.Outdated = true
			info.URL = "https://github.com/" + releaseRepo + "/releases/latest"
		}
	}
	writeJSON(w, http.StatusOK, info)
}

// latestRelease 查远端最新 release tag，进程内缓存 1 小时；失败静默（detail 记原因）。
func (a *API) latestRelease(ctx context.Context) (latest, detail string) {
	releaseCacheMu.Lock()
	defer releaseCacheMu.Unlock()
	if time.Since(releaseCacheAt) < time.Hour && (releaseCache.Latest != "" || releaseCache.Detail != "") {
		return releaseCache.Latest, releaseCache.Detail
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+releaseRepo+"/releases/latest", nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		releaseCache = versionInfo{Detail: "查询失败: " + err.Error()}
		releaseCacheAt = time.Now()
		return "", releaseCache.Detail
	}
	defer resp.Body.Close()
	var body struct {
		TagName string `json:"tag_name"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil || body.TagName == "" {
		releaseCache = versionInfo{Detail: "查询失败: " + resp.Status}
		releaseCacheAt = time.Now()
		return "", releaseCache.Detail
	}
	releaseCache = versionInfo{Latest: body.TagName}
	releaseCacheAt = time.Now()
	return body.TagName, ""
}
