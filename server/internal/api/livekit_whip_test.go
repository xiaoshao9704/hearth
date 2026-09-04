// 外部 / 远端 livekit 实例的推流面：推流经 hearth 反代到该实例自带的 /whip/v1。
// 这条接线就是 cmd/stage 远端形态在 hearth 侧的全部内容——区别只在上游 LiveKit
// 跑在哪台机器上，测试里让它跑在本机（进程内实例的回环端口）。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"hearth/server/internal/lkroom"
	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

var livekitLocRe = regexp.MustCompile(`^/providers/stage1/w/sessions/[0-9a-f]{32}$`)

// 注册一个 livekit 类型实例指向已跑起来的 LiveKit，stage_provider 选它（推流无独立
// 选择器，一律进当前舞台实例自带的 WHIP 入口），推流 201 → 观众名册里出现 kind=ingest
// 的推流设备 → DELETE 后消失。此时 lkembed 不再是当前舞台实例，/providers/lkembed/w 应 404。
// api_url 故意写 ws:// 形式：验 whipBase 的 scheme 归一（照抄浏览器信令地址的常见填法）。
func TestLivekitInstanceWhipEndToEnd(t *testing.T) {
	a, base, lkPort := stageAPI(t)
	ctx := context.Background()
	if err := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "stage1", Type: TypeLivekit, Params: map[string]string{
		"livekit_api_url":    fmt.Sprintf("ws://127.0.0.1:%d", lkPort),
		"livekit_api_key":    a.dynVal(ctx, "lkembed_api_key"),
		"livekit_api_secret": a.dynVal(ctx, "lkembed_api_secret"),
	}}); err != nil {
		t.Fatalf("注册 livekit 实例失败: %v", err)
	}
	a.reloadProviders(ctx)
	if msg := a.checkSelector(ctx, "stage_provider", "stage1"); msg != "" {
		t.Fatalf("stage_provider=stage1 应合法: %s", msg)
	}
	if err := a.st.SetSetting(ctx, "cfg_stage_provider", "stage1"); err != nil {
		t.Fatalf("写选择器失败: %v", err)
	}
	if alias, ip := a.ingestInstance(ctx); alias != "stage1" || ip == nil {
		t.Fatalf("推流入口应跟随舞台实例 stage1，实际 %q", alias)
	}

	u, it := seedIngestUser(t, a, "dave", "chan1")
	identity := rtc.Identity(u.ID, "obs")
	// stage1 的 api_url 故意写的 ws://（验 WHIP 换票的 scheme 归一），实例对象的 Twirp
	// 管理面走不通这个 scheme；参与者断言因此直连 LiveKit 列名册，验的是上游真实状态
	lkc := lkroom.NewClient(fmt.Sprintf("http://127.0.0.1:%d", lkPort),
		a.dynVal(ctx, "lkembed_api_key"), a.dynVal(ctx, "lkembed_api_secret"))
	waitIdentity := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			found := false
			if ps, err := lkc.ListParticipants(ctx, "chan1"); err == nil {
				for _, p := range ps {
					if p.Identity == identity {
						found = true
						var meta rtc.Meta
						if err := json.Unmarshal([]byte(p.Metadata), &meta); err == nil &&
							(meta.UID != u.ID || meta.Kind != "ingest" || meta.Tag != "obs") {
							t.Fatalf("推流参与者元数据不符: %+v", meta)
						}
					}
				}
			}
			if found == want {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("等待 %s 出现/消失超时（want=%v）", identity, want)
	}

	// 旧默认 alias：lkembed 已不是当前舞台实例，推流应 definitive 404
	if c := whipPush(t, base, "/providers/lkembed/w/chan1/"+it.Token); c.code != http.StatusNotFound {
		t.Fatalf("非当前舞台实例推流应 404，实际 %d: %s", c.code, c.body)
	}

	c := whipPush(t, base, "/providers/stage1/w/chan1/"+it.Token)
	defer c.close()
	if c.code != http.StatusCreated {
		t.Fatalf("推流应 201，实际 %d: %s", c.code, c.body)
	}
	if !livekitLocRe.MatchString(c.location) {
		t.Fatalf("Location 应为 /providers/stage1/w/sessions/{会话id}，实际 %q", c.location)
	}
	if !strings.Contains(c.body, "m=video") {
		t.Fatalf("answer 应含视频 m-line: %s", c.body)
	}
	waitIdentity(true)

	req, _ := http.NewRequest("DELETE", base+c.location, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE 应 204，实际 %d", resp.StatusCode)
	}
	waitIdentity(false)
}

// livekit 实例三面齐全：语音/舞台两个选择器都能选中它，推流面（Ingest）随之可用。
func TestLivekitInstanceCaps(t *testing.T) {
	maskProviderEnv(t)
	a := testAPI(t)
	ctx := context.Background()
	if err := a.st.CreateProvider(ctx, &store.ProviderRecord{Alias: "stage1", Type: TypeLivekit, Params: map[string]string{
		"livekit_api_url":    "http://127.0.0.1:7880",
		"livekit_api_key":    "k",
		"livekit_api_secret": "s",
	}}); err != nil {
		t.Fatalf("注册实例失败: %v", err)
	}
	a.reloadProviders(ctx)
	inst := a.instance("stage1")
	if inst == nil || inst.Voice == nil || inst.Stage == nil || inst.Ingest == nil {
		t.Fatalf("livekit 实例应同时具备 voice/stage/ingest 能力: %+v", inst)
	}
	if !inst.Ingest.Enabled(ctx) {
		t.Fatal("配置齐全的外部 livekit 实例推流面应可用")
	}
}
