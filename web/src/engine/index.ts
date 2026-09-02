// 引擎注册表：按凭证里的 engine 名动态加载实现（SDK 体积只在用到时下载）。
import type { AVEngine, EngineCallbacks } from './types';

const registry: Record<string, (cbs: EngineCallbacks) => Promise<AVEngine>> = {
  livekit: async (cbs) => new (await import('./livekit')).LiveKitEngine(cbs),
  'ember': async (cbs) => new (await import('./ember')).EmberEngine(cbs),
};

export async function createEngine(name: string, cbs: EngineCallbacks): Promise<AVEngine> {
  const make = registry[name] ?? registry['livekit'];
  return make(cbs);
}
