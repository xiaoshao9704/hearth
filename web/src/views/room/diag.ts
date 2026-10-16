// 房间诊断：会话 id、事件上报，以及线路读数 / 观看采样定时器。
// 信号与引擎仍由房间页持有，本模块只吃参数、不另造真相源。
import { createEffect, onCleanup } from 'solid-js';
import { reportClientLog } from '../../api';
import type { AVEngine, WatchCounters } from '../../engine/types';
import { addWatch, diffWatch, emptyWatchTotals, shouldReportWatch, watchLevel } from '../../watchdiag';
import type { WatchTotals } from '../../watchdiag';

type Role = 'voice' | 'stage';

export type RoomDiag = (
  level: 'info' | 'warn' | 'error',
  event: string,
  role?: Role,
  fields?: Omit<Parameters<typeof reportClientLog>[0], 'level' | 'event' | 'session' | 'channel' | 'role'>,
) => void;

export function createRoomDiag(channel: string): {
  diag: RoomDiag;
  endpointOf: (raw: string) => string;
} {
  const diagBytes = new Uint8Array(8);
  if (globalThis.crypto?.getRandomValues) globalThis.crypto.getRandomValues(diagBytes);
  else for (let i = 0; i < diagBytes.length; i++) diagBytes[i] = Math.floor(Math.random() * 256);
  const diagSession = Array.from(diagBytes, (b) => b.toString(16).padStart(2, '0')).join('');

  const diag: RoomDiag = (level, event, role, fields = {}) =>
    reportClientLog({ level, event, session: diagSession, channel, role, ...fields });

  const endpointOf = (raw: string): string => {
    try {
      return new URL(raw, location.href).host;
    } catch {
      return '';
    }
  };

  return { diag, endpointOf };
}

export interface LineDiagOpts {
  diag: RoomDiag;
  voicePhase: () => string;
  combined: () => boolean;
  voiceEngine: () => AVEngine | null;
  stageEngine: () => AVEngine | null;
}

// 线路读数：第一次等 10 秒，之后每 60 秒；返回 dispose。
export function startLineDiag(opts: LineDiagOpts): () => void {
  let lineDiagTimer = 0;
  const tick = async () => {
    if (document.hidden || opts.voicePhase() !== 'up') return;
    const rows: [Role, AVEngine | null][] = opts.combined()
      ? [['voice', opts.voiceEngine()]]
      : [
          ['voice', opts.voiceEngine()],
          ['stage', opts.stageEngine()],
        ];
    for (const [role, eng] of rows) {
      if (!eng?.connected()) continue;
      const line = eng.lineStats();
      const video = opts.combined() || role === 'stage' ? await eng.screenStats() : null;
      if (!line && !video) continue;
      opts.diag('info', 'line_stats', role, { detail: JSON.stringify({ line, video }).slice(0, 2000) });
    }
  };
  const start = window.setTimeout(() => {
    void tick();
    lineDiagTimer = window.setInterval(() => void tick(), 60000);
  }, 10000);
  return () => {
    clearTimeout(start);
    clearInterval(lineDiagTimer);
  };
}

export interface WatchDiagOpts {
  diag: RoomDiag;
  enabled: () => boolean;
  stageEngine: () => AVEngine | null;
  screenEntries: () => { key: string; identity: string; source: string; isLocal: boolean }[];
  setWatchTotals: (fn: (t: WatchTotals | null) => WatchTotals | null) => void;
}

// 观看诊断：开关关着时不建定时器；在 createEffect 里调用，由 Solid 的 onCleanup 回收。
export function wireWatchDiag(opts: WatchDiagOpts): void {
  const watchState = new Map<string, { prev: WatchCounters; reportedAt: number }>();
  createEffect(() => {
    if (!opts.enabled()) {
      watchState.clear();
      opts.setWatchTotals(() => null);
      return;
    }
    const tick = async () => {
      const eng = opts.stageEngine();
      if (!eng?.connected()) return;
      const targets = opts.screenEntries().filter((e) => e.source === 'screen' && !e.isLocal);
      for (const key of [...watchState.keys()]) if (!targets.some((e) => e.key === key)) watchState.delete(key);
      for (const entry of targets) {
        const cur = await eng.remoteWatchCounters(entry.identity, 'screen');
        if (!cur) continue;
        const prevState = watchState.get(entry.key);
        const sample = prevState ? diffWatch(prevState.prev, cur) : null;
        let reportedAt = prevState?.reportedAt ?? 0;
        if (sample) {
          opts.setWatchTotals((t) => addWatch(t ?? emptyWatchTotals(), sample));
          const now = Date.now();
          if (shouldReportWatch(sample, now - reportedAt)) {
            reportedAt = now;
            opts.diag(watchLevel(sample), 'watch_stats', 'stage', {
              detail: JSON.stringify({ me: eng.localIdentity(), from: entry.identity, ...sample }).slice(0, 2000),
            });
          }
        }
        watchState.set(entry.key, { prev: cur, reportedAt });
      }
    };
    const timer = window.setInterval(() => void tick(), 5000);
    onCleanup(() => window.clearInterval(timer));
  });
}
