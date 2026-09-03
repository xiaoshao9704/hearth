// 外部 / 远端 livekit 实例的推流面：推流经 hearth 反代到该实例自带的 /whip/v1。
// 这条接线就是 cmd/stage 远端形态在 hearth 侧的全部内容——区别只在上游 LiveKit
// 跑在哪台机器上，测试里让它跑在本机（进程内实例的回环端口）。
package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"hearth/server/internal/rtc"
	"hearth/server/internal/store"
)

var livekitLocRe = regexp.MustCompile(`^/providers/stage1/w/sessions/[0-9a-f]{32}$`)

// 注册一个 livekit 类型实例指向已跑起来的 LiveKit，ingest_provider 选它，
// 推流 201 → 观众名册里出现 kind=ingest 的推流设备 → DELETE 后消失。
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
	if msg := a.checkSelector(ctx, "ingest_provider", "stage1"); msg != "" {
		t.Fatalf("ingest_provider=stage1 应合法: %s", msg)
	}
	if err := a.st.SetSetting(ctx, "cfg_ingest_provider", "stage1"); err != nil {
		t.Fatalf("写选择器失败: %v", err)
	}
	if alias, _, fellBack := a.ingestInstance(ctx); alias != "stage1" || fellBack {
		t.Fatalf("推流入口应为 stage1，实际 %q（回落=%v）", alias, fellBack)
	}

	u, it := seedIngestUser(t, a, "dave", "chan1")
	identity := rtc.Identity(u.ID, "obs")
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
	waitParticipant(t, a, "chan1", identity, true)
	for _, p := range stageParticipants(t, a, "chan1") {
		if p.Identity == identity && (p.UID != u.ID || p.Kind != "ingest" || p.Tag != "obs") {
			t.Fatalf("推流参与者信息不符: %+v", p)
		}
	}

	req, _ := http.NewRequest("DELETE", base+c.location, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE 应 204，实际 %d", resp.StatusCode)
	}
	waitParticipant(t, a, "chan1", identity, false)
}

// livekit 实例三面齐全：语音/舞台/推流三个选择器都能选中它。
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
