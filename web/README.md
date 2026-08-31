# FlashCast Web

浏览器前端：Vite + 原生 TypeScript（无框架），`livekit-client` 接入音视频。

页面（hash 路由）：

- `#/login` 登录 / 注册
- `#/channels` 频道列表，可创建频道
- `#/room/<频道名>` 房间页：麦克风 / 摄像头 / **投屏** 开关（进房默认全关，按 localStorage 偏好恢复）；九宫格/聚焦布局切换；参与者列表（含本地静音）+ 视频画面；侧边聊天面板（WebSocket，含最近 50 条历史）；「⚙ 设置」面板含投屏画质（分辨率/帧率/码率）、RNNoise 降噪与音频处理开关、麦克风设备与语音码率

## 投屏参数（核心诉求）

`src/views/room.ts`：分辨率 1080p/720p、帧率 60/30/15、码率滑块 1–15 Mbps（默认按 宽×高×帧率×0.07 的 bpp 模型推导，1080p60≈8.7M）；发布固定 `videoCodec: 'h264'` + `screenShareSimulcastLayers: []`（单层，省 CPU）。选项持久化在 localStorage `fc_room_prefs`，对下一次开启投屏生效。

## 音频处理

- RNNoise AI 降噪：`@sapphi-red/web-noise-suppressor`（AudioWorklet + WASM，资源由 vite 打包），`src/audio.ts` 封装管线；加载失败自动回退浏览器内置处理并置灰开关
- 浏览器内置处理（回声消除/降噪/自动增益）独立开关，另有"音乐模式"一键全关 + 语音 128k
- 语音码率档 32k/64k/96k/128k，走 publish 的 `audioPreset.maxBitrate`

## 开发

```bash
cp .env.example .env   # 配置 server 地址与 LiveKit WSS 兜底地址
npm install
npm run dev            # http://localhost:5173
```

## 构建

```bash
npm run build          # tsc 类型检查 + vite build，产物在 dist/
```

## 环境变量（.env）

- `VITE_SERVER_URL`：后端 API 地址（默认 `http://localhost:8080`）
- `VITE_LIVEKIT_URL`：LiveKit WSS 兜底地址（优先使用 server `/api/token` 下发的 `url`）
