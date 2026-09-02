# 计划：功能键全面改用 user_id，username 只留展示/登录/注册

状态：待实施。前置：`plan-ingest-token.md` 已实施（本计划复用它建好的参与者元数据通道）。

## 动机

`store` 与 `chat` 两层早就是 user_id 键（`channel_bans`/`channel_gags`/`channel_members`/`devices`/
`ingest_tokens` 全是 `user_id`，`Hub.CloseUserChannel(userID, channelID)`）。唯一还拿 username 当功能键的是
**rtc 边界**：identity 由用户名拼成，归属判断靠字符串前缀。由此产生两类现实错误：

1. **改名后身份错位**。`ingest_endpoints` 里 livekit-ingress 端点的 `ParticipantIdentity`/`ParticipantName`/
   `metadata.username` 在建端点时固化。用户改名后端点被原样复用，推流仍以旧用户名进房：房主对新用户名的
   踢出/禁言全部落空。旧用户名改名后即释放，被他人注册后，这条流会顶着别人的身份出现在房间里并受其管制操作影响。

2. **前缀歧义误伤第三方**。`usernameRe` 是 `^[a-zA-Z0-9_-]{2,32}$`，用户名允许 `-`；
   `MatchesUser` 又是 `identity == username || HasPrefix(identity, username+"-")`。于是用户 A=`alice`
   （推流 tag=`obs`，identity `alice-obs`）与用户 B=`alice-obs` 共存时，禁言 B 会掐掉 A 的推流，
   踢 A 会连带断掉 B 的浏览器会话。`plan-ingest-token.md` 把 tag 变成用户可改属性后，这个构造面从
   写死的 `-obs` 扩大到任意合法标签。

根因一条：**username 是可变、可释放、且字符集与分隔符冲突的标识，不该进入任何判定路径。**
`user_id` 恒定、唯一、不可重用，本就是系统的认人依据（设置页文案已经这么写了，只是实现没做到）。

## 决策点（执行前确认）

- **identity 形状 `u{id}[-{tag}]`**：浏览器 `u17-mac-a1b2c3`（tag 沿用 `deviceTagFor` 的
  `{UA标签}-{设备id}`），推流 `u17-obs`。`u` 前缀只为日志可读，不承担语义。identity 不落库、
  纯运行时值，因此**没有 schema 迁移**；在途会话随升级重启重连自然收敛。
- **HTTP 契约一并换成 `{user_id}`**（踢出/封禁/禁言的请求体）。username 从此只出现在登录、注册、
  改名与展示三处，不再是任何接口的选择器。前端右键菜单的目标 user_id 由参与者元数据给出（见下条），
  不需要额外查询。破坏性变更，不留兼容——重新部署即同步更新前后端。
- **参与者元数据成为唯一的身份信息通道**。`plan-ingest-token.md` 已为推流参与者建好
  `{username,kind,tag}` JSON；本计划把它扩到**全部**参与者，并加 `uid` 字段。
  前端从此既不解析 identity，也不靠用户名反查——右键菜单、禁言、踢出全部直接用 `uid`。

## 改动清单

### S1 rtc 边界换键（`server/internal/rtc/`）

- 新增 `rtc.Identity(userID int64, tag string) string`，identity 组装收敛到这一处。
- `MatchesUser(identity string, username string)` → `MatchesUser(identity string, userID int64)`，
  按 `u{id}` 段精确比对，不再做用户名前缀匹配。
- `Provider.RemoveParticipantsOf(ctx, room, username)` → `(ctx, room string, userID int64)`；
  `Provider.MuteUserAudio(ctx, room, username, muted)` → `(ctx, room string, userID int64, muted bool)`。
- 调用点四处：`lkroom.go:100`、`lkroom.go:129`、`ember.go:204`、`ember.go:229`。
- `rtc.Participant` 补 `Username` 字段（展示用，来自元数据）。

### S2 identity 组装点改造（`server/internal/api/`、`server/internal/lktoken/`）

- `lktoken.Sign`：`SetIdentity(rtc.Identity(userID, device))`、`SetName(username)` 保持，
  新增 `SetMetadata({uid,username,kind:"",tag})`——浏览器参与者也带元数据。
- `admission.go:36` `admitUser` 的 `admission` 结构：`Identity` 改由 user_id 生成，另留 `Username` 供展示。
- `admission.go:159` `admitIngest`：`Identity: rtc.Identity(u.ID, it.Tag)`，`Name` 仍是用户名（显示名）。
- `api.go:583` `evict`：传 `t.ID` 而非 `t.Username`。
- `api.go:621` 设备级踢出校验：`MatchesUser(req.Identity, t.ID)`。
- `api.go:692` 禁言传播：传 `t.ID`。
- `resolveTargetUser` 改收 `{user_id}`：`UserByID` 取代 `UserByName`，自我校验从
  `req.Username == u.Username` 改成 `req.UserID == u.ID`。

### S3 前端改用 uid（`web/src/`）

- `EPart` / `RoomParticipant` 补 `uid`，与 `username`/`tag` 一样对**每个**参与者由元数据/名册给出。
- 删除五处 identity 解析：`manage.ts:81`、`engine/livekit.ts:50`、
  `engine/ember.ts:22`（`usernameOf` 整个删除）、`room.tsx:284`、`room.tsx:1238`。
- 右键菜单与管理页的踢出/封禁/禁言改发 `{user_id}`；`api.ts` 对应函数签名同步换成 `uid: number`。
- 成员聚合键从 username 换成 uid（同名不同人的歧义随之消失）。

### S4 存量端点失效重建（游标迁移 v4）

`ingest_endpoints` 里的端点带旧 `{用户名}-{标签}` identity，升级后必须重建。新增游标步 v4：
逐实例 `DeleteEndpoint` 后清空 `ingest_endpoints`，下次推流按新 identity 惰性重建
（复用 `teardownIngestEndpoints` 的逻辑，对全表执行）。

> 副作用：动机 §1 的改名 bug 由此**结构性消失**——identity 不含用户名，改名不再让端点过期，
> 只有改标签才需要重建（现有 `ingestTokenTag` 已经在做）。

## 验收

- `cd server && go build ./... && go vet ./... && go test ./...` 全过。
- `cd web && npx tsc --noEmit && npm run build` 全过。
- 新增回归测试：
  - `MatchesUser` 对「用户名含 `-` 且互为前缀」的两个用户不再互相命中（动机 §2 的构造）。
  - 改名后推流 identity 不变、房主对新用户名的禁言/踢出仍命中。
  - 游标 v4 幂等：重跑不重复删端点、空表直接通过。
- 本地双线冒烟：ember 语音 + livekit 舞台，验证名册显示名、设备名、禁言/踢出、OBS 推流角标均正常。

## 不做

- 不改 `users.username` 的唯一约束与改名策略（旧名释放后可被注册，这在 user_id 模型下不再有害）。
- 不动 `listUsernames`（管理后台的封禁/白名单列表按 user_id 查、转成用户名展示，属正确用法）。
- 不改聊天消息的作者字段（已是 user_id 外键）。
