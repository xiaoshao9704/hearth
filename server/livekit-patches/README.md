# livekit-server 补丁序列

hearth 进程内嵌入的 LiveKit 用的是 `livekit/livekit-server` 的**补丁式 fork**：不改架构，只在上游 tag 上贴这里的几个补丁
（方案见 `docs/plan-livekit-embed.md`）。本目录是补丁的权威副本，fork 仓库可随时从它重建。

| 补丁 | 行数 | 作用 |
| --- | --- | --- |
| `0001-*` | 1 | 删掉 `pkg/rtc/transport.go` 里 `se.EnableSped(true)`：它只存在于 LiveKit 自家的 pion 分叉，受 `enable_warp`（默认关）门控；删掉后整个服务端对**上游** pion 编译 |
| `0002-*` | 21 | `config.RTCConfig.ExternalIPs func() []string`（非 YAML 字段）→ 透传到 `rtc.WebRTCConfig` → 每建一个 transport 时在其 `SettingEngine` 副本上 `SetNAT1To1AddressRewriteRules(&se, ips, true)`。宿主注入回调后，公网 IP / 端口映射变化对新会话即时生效、不重启；`true` = 追加，保留本机 host 候选 |

基线：`v1.13.6`（上游 pion `webrtc/v4 v4.2.18`、`ice/v4 v4.4.0`、`dtls/v3 v3.1.5`）。

## 重建 fork

上游仓库现在叫 `livekit/livekit`（旧地址 `livekit/livekit-server` 是重定向）；**Go module 路径仍是
`github.com/livekit/livekit-server`**（`replace` 要求 fork 的 go.mod module 与原路径一致，所以 fork 里不改它）。
fork 仓库名建议与上游对齐，也叫 `livekit`。

```sh
git clone --branch v1.13.6 https://github.com/livekit/livekit.git && cd livekit
git checkout -b hearth-patches
git am <hearth 仓库>/server/livekit-patches/*.patch
git tag -a v1.13.6-hearth.1 -m "livekit-server v1.13.6 + hearth patches 1-2"
git remote add fork git@github.com:<你的账号>/livekit.git
git push fork hearth-patches v1.13.6-hearth.1
```

hearth 侧 `server/go.mod`：

```
require github.com/livekit/livekit-server v1.13.6
replace github.com/livekit/livekit-server => github.com/<你的账号>/livekit v1.13.6-hearth.1
```

fork 的 go.mod 里指向 `-warp` 分叉的 `replace` 对依赖方无效，hearth 自动用上游 pion，这正是补丁一存在的原因。

## 跟上游

升级 = 新 tag 上 `git rebase`（两个补丁、三个文件，冲突概率很低）→ 重新打 `vX.Y.Z-hearth.N` → 同时改 hearth 的 `require`/`replace` →
重跑 `docs/plan-livekit-embed.md` 的第 1 与第 4 步验收，并重做一次分叉 diff 确认上游分叉没长出新的非 warp 改动。
