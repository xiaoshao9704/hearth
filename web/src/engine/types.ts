// AVEngine：房间音视频引擎的内核中立接口。
// 房间视图（room.ts）只依赖本接口；每个内核（livekit / 将来的 pion-voice …）配一个实现，
// 由进房凭证里的 engine 字段选择、动态加载。信令协议是各内核私有的，全部封装在实现里。

export type TrackSource = 'camera' | 'screen';

// 参与者快照（identity 粒度 = 账号的一台设备；username 用于按账号聚合展示）
export interface EPart {
  identity: string;
  username: string;
  display: string;
  isLocal: boolean;
  micOn: boolean;
  sharing: boolean; // 有投屏轨
  obs: boolean; // OBS 推流参与者（identity 以 -obs 结尾）
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
  // 连接终结。lost = 引擎放弃恢复，房间层负责拿新凭证重连；其余为终态
  onEnded(reason: 'kicked' | 'room-deleted' | 'duplicate' | 'lost'): void;
  onLocalTrackEnded(kind: 'mic' | 'camera'): void; // 采集设备中途断开（如连续互通断开）
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
  restartMic(): Promise<void>; // 开麦状态下设备/处理链变更：重启采集
  localMicTrack(): MediaStreamTrack | null; // 当前发布中的本地麦克风轨（本地电平表用；未开麦为 null）
  // 投屏实际生效的编码器（getStats 运行时真值）；未投屏或引擎无视频返回 null。
  // hw: powerEfficientEncoder 标准字段，旧浏览器缺失时为 null（调用方可按 impl 名兜底判断）
  screenEncoderInfo(): Promise<{ impl: string; hw: boolean | null } | null>;
  switchCamera(deviceId: string): Promise<void>;
  dispose(): void; // 离开房间：断开并释放全部采集资源
}
