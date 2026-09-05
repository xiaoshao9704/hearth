// AVEngine：房间音视频引擎的内核中立接口。
// 房间视图（room.ts）只依赖本接口；每个内核配一个实现，
// 由进房凭证里的 engine 字段选择、动态加载。信令协议是各内核私有的，全部封装在实现里。

export type TrackSource = 'camera' | 'screen';

// 视频流运行时真值（getStats）：发送侧为实际上行，接收侧为本端实际拿到的层
export interface VideoStats {
  width: number;
  height: number;
  fps: number;
  kbps: number;
}

// 参与者快照（identity 粒度 = 账号的一台设备；username 用于按账号聚合展示）
export interface EPart {
  identity: string;
  uid: number; // 归属用户 id（内核元数据透传）：管理操作的目标，前端不解析 identity
  username: string;
  display: string;
  isLocal: boolean;
  micOn: boolean;
  canPublish: boolean; // false = 被服务端禁言（channel_gags）：无法开麦/推流
  sharing: boolean; // 有投屏轨
  ingest: boolean; // 推流参与者（参与者元数据 kind=ingest，不再解析 identity 后缀）
  tag: string; // 推流设备标签（identity = {username}-{tag}）；非推流参与者为空
}

// 数据通道话题：文本走 TEXT，文件字节走 FILE。引擎只按 topic 注册/透传，
// 「哪个 topic 装什么」是房间层的约定，两边共用这两个常量避免拼错字符串。
export const DATA_TOPIC_TEXT = 'chat';
export const DATA_TOPIC_FILE = 'chat-file';

// 收到的文件字节的附带信息（attrs 是发送方挂的自定义键值，房间层用 message_id 关联卡片）
export interface DataFileInfo {
  name: string;
  mime: string;
  size: number;
  attrs: Record<string, string>;
}

// 引擎 → 房间视图的事件回调。引擎负责把媒体轨变成可挂载的元素；
// 音量/静音/挂载位置等呈现层决策全部留在房间视图。
export interface EngineCallbacks {
  onVideoTrack(part: EPart, source: TrackSource, el: HTMLVideoElement): void;
  onVideoTrackRemoved(identity: string, source: TrackSource, els: HTMLMediaElement[]): void;
  onAudioTrack(identity: string, el: HTMLMediaElement): void;
  onAudioTrackRemoved(identity: string, els: HTMLMediaElement[]): void;
  onRoster(): void; // 参与者进出 / 麦克风状态变化，视图重取 participants() 重绘
  onSpeakers(identities: string[]): void;
  onReconnecting(): void; // 引擎内部在自行恢复
  onReconnected(): void;
  onDiagnostic?(event: string, state: string, detail?: string): void; // SDK 状态流，仅供脱敏后的故障诊断上报
  // 连接终结。lost = 引擎放弃恢复，房间层负责拿新凭证重连；其余为终态
  onEnded(reason: 'kicked' | 'room-deleted' | 'duplicate' | 'lost'): void;
  onAudioBlocked?(): void; // 浏览器拦截了自动播放：需要用户手势才能出声
  onLocalTrackEnded(kind: 'mic' | 'camera' | 'screen'): void; // 采集中途终止：设备断开（如连续互通断开）、浏览器原生「停止共享」
  // 数据通道到达（内核不解释内容，原样上交）；自己发的不会回显
  onText?(topic: string, text: string, fromIdentity: string): void;
  // bytes 显式标注非共享 buffer：调用方要直接喂 Blob，泛型省掉 lib.dom 的 SharedArrayBuffer 分支
  onFile?(topic: string, info: DataFileInfo, bytes: Uint8Array<ArrayBuffer>, fromIdentity: string): void;
}

export interface AVEngine {
  connect(url: string, token: string): Promise<void>;
  disconnect(): void;
  connected(): boolean;
  localIdentity(): string;
  participants(): EPart[];
  // 发布控制：采集参数（设备、降噪链、码率、编码）由实现按 prefs 读取
  setMic(on: boolean): Promise<void>;
  setCamera(on: boolean): Promise<void>;
  setScreen(on: boolean): Promise<void>;
  // 投屏进行中按 prefs 重设画质：码率/帧率/分辨率就地改在途轨，换编码则重新发布
  //（采集轨保留，不用重选窗口；观众端短暂重订阅）。返回是否发生了重新发布；未投屏返回 false
  applyScreenPrefs(): Promise<boolean>;
  restartMic(): Promise<void>; // 开麦状态下设备/处理链变更：重启采集
  localMicTrack(): MediaStreamTrack | null; // 当前发布中的本地麦克风轨（本地电平表用；未开麦为 null）
  // 投屏实际生效的编码器（getStats 运行时真值）；未投屏或引擎无视频返回 null。
  // hw: powerEfficientEncoder 标准字段，旧浏览器缺失时为 null（调用方可按 impl 名兜底判断）
  screenEncoderInfo(): Promise<{ impl: string; hw: boolean | null } | null>;
  // 本地投屏的实测发送数据（getStats 差分码率）；未投屏或引擎无视频返回 null
  screenStats(): Promise<VideoStats | null>;
  // 远端视频轨的本端实测接收数据（SVC 下反映本端实际拿到的层）；无该轨或引擎无视频返回 null
  remoteVideoStats(identity: string, source: TrackSource): Promise<VideoStats | null>;
  switchCamera(deviceId: string): Promise<void>;
  resumeAudio(): Promise<void>; // 用户手势后重放被拦截的音频元素
  // 数据通道发送：未连接时抛错，调用方先看 connected()
  sendText(topic: string, text: string): Promise<void>;
  // attrs 随字节一起送达接收方（房间层挂 message_id 关联卡片）；onProgress 收 0~1
  sendFile(file: File, topic: string, attrs: Record<string, string>, onProgress?: (p: number) => void): Promise<void>;
  dispose(): void; // 离开房间：断开并释放全部采集资源
}
