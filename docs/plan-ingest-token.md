# 计划：推流令牌改为用户维度（房间在 URL 里）+ 推流设备标签 + Bellows 出口抽象

状态：已实施（I1–I7 全过：迁移测试、两进程冒烟 8/8、终审可合入。令牌按用户要求为每用户一把、不区分设备）。2026-09-02。前置：`plan-provider-registry-fixes.md` 已实施；本计划取代其 §5、§6
（令牌不再绑定房间与实例，那两条保护的前提消失）。

## 动机

现状推流密钥是「用户 × 频道」一把，换房间要换密钥、OBS 里重新粘贴；identity 的 `-obs` 后缀由 Bellows 硬编码，
业务决策落在内核里；「这是 OBS」靠前端解析 identity 后缀。三件事一起理顺：

1. **令牌用户维度**：一个用户一把令牌，**不区分频道和设备**，房间名放在 URL 路径里。换房间只改 URL，令牌不动。
2. **推流设备标签**：标签是这把令牌的可改属性（默认 `obs`），identity = `{用户名}-{标签}`，
   由 hearth 组好经通行证下发，Bellows 与内核只透传。"是否推流设备"走参与者元数据，不再解析后缀。
3. **Bellows 出口抽象**：`rtc.Publisher` 能力接口，Bellows 对舞台内核中立；LiveKit 的发布代码搬回 `livekitrtc`。

## 决策点（执行前确认）

- **`livekit-ingress` 类型保留，按「用户令牌 → 实例凭证」模型接入**。LiveKit 的 Ingress 对象本身就是"一个发布参与者"
  （identity / name / metadata），与"用户 + 设备"一一对应；房间是它的一个**可更新**属性（`UpdateIngressRequest.room_name`），
  不是创建时的死绑定。因此 hearth 在反代前把房间写进去即可：
  - 每（用户令牌, 实例）惰性创建一个 Ingress 对象，identity `{用户名}-{标签}`，记录 `ingress_id` 与 LiveKit 签发的
    stream key（表 `ingest_endpoints(token_id, alias, ingress_id, upstream_key)`）；
  - POST 到达：`admitIngest` 通过后，若记录里的 `bound_room` ≠ URL 频道才调 `UpdateIngress(room_name)` 并更新
    `bound_room`（稳态推流零控制面调用，控制面短暂不可达不影响推回同一房间），再把请求头 `Authorization: Bearer <用户令牌>`
    **改写**为上游的 stream key 后反代——hearth 终结用户令牌，向上游出示实例凭证，与 Bellows 的通行证是同一件事的两种形态；
  - 并发语义差异要写进文档：同一令牌同时推两个房间，Bellows 是新会话顶掉旧会话，LiveKit Ingress 是一个对象同时只能活动一次、
    第二路被上游拒绝。两者都满足"一台设备同时只推一个房间"，只是失败的一方不同，hearth 不强行统一。
  - 令牌重置/标签改名：删除该令牌名下全部实例端点（`DeleteIngress`），下次推流重建。
  代价：一次 Twirp 调用/每次推流；换来的是 LiveKit Ingress 独有的 RTMP/URL 拉流输入与转码能力仍可用。
  接口上 `rtc.IngestProvider` 因此保留端点方法，但语义改为按（identity, 标签）而非（用户, 房间）：
  `EnsureEndpoint(ctx, identity, name, meta) (id, upstreamKey)`、`BindRoom(ctx, id, room)`、`DeleteEndpoint(ctx, id)`；
  Bellows 的三个方法都是空操作（它的实例凭证就是通行证）。
- 令牌**每用户一把，不区分设备**：标签是它的可改属性（改标签即改下次推流的 identity）。今后有第二路需求再做端点列表。

## 令牌与 URL

- 表 `ingest_tokens`：`user_id`、`tag`（`^[a-z0-9][a-z0-9-]{0,31}$`，可改，默认 `obs`）、`token`（64 位 hex，32 字节随机）、`created_at`；
  `UNIQUE(user_id)`——**每用户一把，不区分频道和设备**。不再有 `provider`/`ingress_id`/`channel_id` 列——令牌是用户凭证，与实例、房间无关。
- WHIP 端点两种填法（与现状对称，`{alias}` 是推流入口实例）：
  - bearer 模式（OBS）：服务器 `/providers/{alias}/w/{channel}`，Bearer 填令牌；
  - 路径模式（ffmpeg 等）：`/providers/{alias}/w/{channel}/{token}`。
- 应答 `Location: /providers/{alias}/w/sessions/{rid}`（会话资源 id，不含令牌）；客户端 PATCH/DELETE 打这个地址。
  hearth→远端 Bellows 的撤销端点改名 `/w/revoke/{token}`（仍走 revoke 通行证）。
  因此 **`sessions`、`revoke` 成为保留频道名**：`channelNameRe` 校验通过后再拒绝这两个字面值。
- `rtc.WHIPToken` 改为解析 `(channel, token, bearer)`（POST）与 `rid`（`/w/sessions/{rid}`，非 POST）。

## hearth 侧

- **入场判定在推流时刻做**：POST 到达 → 令牌反查用户 + URL 取频道 → `admitUser(channel, user)`（封禁/邀请制/禁言）
  → 通过则按实例类型出示凭证：Bellows 签通行证，`livekit-ingress` 做 `BindRoom` + 改写 Bearer。比现在更贴铁律
  （决策在每次推流时，不在生成令牌时）。原 `canPublishByStreamKey` 与 `admitWhipRemote` 合并为一个
  `admitIngest(ctx, alias, channel, token)`，三条路径（进程内 Bellows / 远端 Bellows / livekit-ingress）共用，都 definitive
  （令牌不存在 404、频道不存在 404、不许推 403、查询出错 503），不再有 fail-open 分支——上游 ingress 收到的已经是
  hearth 出示的实例凭证，它不再承担鉴权，fail-open 的理由消失。
- **通行证 payload** 改为 `{v, op, token, room, identity, name, kind:"ingest", tag, offer, exp}`；`user` 字段删除。
  进程内 Bellows 的 `ResolveFunc` 同步改为返回 `(room, identity, name, tag)`——由 `admitIngest` 组好。
- **API**（旧 `/api/ingress`、`/api/ingress/reset` 删除，前端同步改，不留兼容）：
  - `GET /api/ingest/token` → `{token, tag, base}`（`base` = 当前推流入口实例的 `/providers/{alias}/w/`；无令牌时自动创建，标签 `obs`）；
  - `POST /api/ingest/token/reset` → 换令牌（撤销当前会话：对进程内直接 `RevokeToken`，远端经 `RevokeRemoteSessions`）；
  - `PUT /api/ingest/token` `{tag}` → 改标签（只校验标签正则——每用户一把模型下标签没有唯一性对象可冲突；正在推流的会话不掐，下次推流生效）。
- `rtc.IngestProvider`：`Name`、`Enabled`、`ProxyUpstream`、`RevokeToken(ctx, token)`，加上按设备语义的
  `EnsureEndpoint`/`BindRoom`/`DeleteEndpoint`（见决策点；Bellows 空实现）。`ingressURL`、`deleteOldEndpoint`、
  归属自愈、`ingresses.provider` 相关逻辑全部删除；`livekit-ingress` 的上游凭证由 `ingest_endpoints` 表按（令牌, alias）持有，
  反代前改写 Bearer。
- **迁移 v2**（挂在 `migration_version` 游标上）：建 `ingest_tokens`；每个用户取其 `ingresses` 里**最近创建**的一条记录的
  `stream_key` 作为令牌（标签 `obs`），其余丢弃；然后 `DROP TABLE ingresses`。发布说明写明：一个用户原来在多个频道
  的多把密钥合并为一把，OBS 里改服务器地址（加频道段）即可。

## Bellows 侧（`rtc/bellows`）

- 会话按令牌顶替：同一令牌新 POST（无论目标房间）顶掉旧会话——"一台设备同时只推一个房间"。`closeSessionsByKey`
  改名 `closeSessionsByToken`；`RevokeToken` 即它。
- 不再拼 identity：会话从通行证/`ResolveFunc` 拿 `room, identity, name, tag`。
- **出口改走 `rtc.Publisher`**：
  ```go
  // Publisher 是「从这里把轨发布进某个舞台内核」的**客户端**能力，由各内核的接入适配器实现：
  // LiveKit 用 lksdk 走网络，进程内 Ember 直接写房间，将来的远端 Ember 走它自己的发布协议。
  // 它挂在注册表的实例对象上（实例对象本来就是内核的客户端适配器），进程内只是网络距离为零的特例；
  // 远端 cmd/bellows 编译进同一批实现，由 BELLOWS_SINK 选用。
  // meta 会作为参与者元数据下发给观众端（至少含 username、kind=ingest、tag）。
  type Publisher interface {
      PublishRemote(ctx context.Context, room, identity, name string, meta map[string]string,
          tr *webrtc.TrackRemote) (unpublish func(), err error)
  }
  ```
  - `livekitrtc.Provider` 实现它：现在 Bellows 里的 `joinRoom`/`PublishTrack`/PLI 桥接/`lksdk` 依赖整体搬过去；
    `ParticipantMetadata` 写 JSON `{"username","kind":"ingest","tag"}`。
  - 进程内 Bellows 向**当前舞台线实例**的 `Publisher` 发布（`stageInstance().Stage.(rtc.Publisher)`），
    `builtinBellowsCfg` 里读 `livekit_*` 的路由逻辑删除；舞台线为 `none` 或实例不实现 `Publisher` 时 `Enabled=false`。
  - 远端 `cmd/bellows`：编译进全部 `Publisher` 实现，`BELLOWS_SINK=livekit`（默认）选用；`LIVEKIT_*` 环境变量归 livekit sink。
- `bellows.ConfigKeys` 只剩 `bellows_udp_port`/`bellows_public_ip`；`RemoteKeys` 不变。
- 包注释里"读 livekit_* 是有意耦合"一段删除——耦合已经不存在。

## 前端

- `EPart` 增加 `tag`；`obs` 改名为 `ingest`（或保留字段名，语义改为 `meta.kind === "ingest"`）；`username` 从元数据取，
  不再 `split('-')[0]`（用户名允许含 `-`，现有写法本来就是错的）。LiveKit 引擎读 `participant.metadata` JSON；Ember 名册
  加 `username`、`kind`、`tag` 字段。
- 设置页推流段：显示标签（可改）、令牌（可重置）、**房间选择器 → 生成完整服务器地址**并可复制；文案更新
  （删掉"HEVC/AV1 服务端不收"，Bellows 已直通）。
- 房间页 OBS 徽标与"踢出该设备"按 `ingest` 与 `tag` 展示。

## 测试

- `WHIPToken` 解析：两种填法、保留名、非 POST 的 rid。
- `admitIngest`：令牌不存在/频道不存在/封禁/禁言/正常，进程内与远端两条路径各一遍。
- 通行证 payload 新字段签验往返；旧格式（有 `user` 无 `identity`）拒绝。
- Bellows：同令牌换房间顶替旧会话；`RevokeToken` 幂等；`Publisher` 用假实现验证 identity/meta 透传。
- 迁移 v2：多频道多密钥合并、`ingresses` 表消失、游标推进；空库不建令牌。
- 前端 `tsc`；两进程冒烟：OBS/ffmpeg 用同一令牌先后推两个频道，第二次推流第一路会话结束、观众端看到
  `{用户名}-{标签}` 带推流徽标；重置令牌后旧令牌 404 且远端会话被掐。

## 部署与兼容

- 两端同版本一起升级（通行证字段变了，旧远端不认新 grant）。
- 升级后 OBS 只需改服务器地址（多一个频道段），令牌沿用迁移保留的那把。
- 与 `plan-provider-registry-fixes.md` 的关系：其 §5（回落不删端点）与 §6（`rec.Provider == alias`）被本计划取代——
  端点与归属列已不存在；其余不变。
