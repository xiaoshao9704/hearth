// 管理后台「服务状态」tab：常驻进程与宿主资源。
import { createSignal, For, onCleanup, Show } from 'solid-js';
import { adminOverview } from '../../api';
import type { AdminOverview } from '../../api';
import { Placeholder } from './shared';

function fmtUptime(s: number): string {
  if (s < 3600) return `${Math.floor(s / 60)} 分钟`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时`;
  return `${Math.floor(s / 86400)} 天`;
}

export function StatusTab() {
  const [ov, setOv] = createSignal<AdminOverview>();
  const [err, setErr] = createSignal('');
  const [updatedAt, setUpdatedAt] = createSignal(0);
  const [now, setNow] = createSignal(Date.now());

  const refresh = () => {
    adminOverview()
      .then((o) => {
        setOv(o);
        setErr('');
        setUpdatedAt(Date.now());
      })
      .catch((e) => setErr((e as Error).message));
  };
  refresh();

  // 后台标签页不轮询；每秒 tick 一下只为了让「更新于 N 秒前」走字
  const poll = setInterval(() => {
    if (document.visibilityState === 'visible') refresh();
  }, 10_000);
  const tick = setInterval(() => setNow(Date.now()), 1000);
  onCleanup(() => {
    clearInterval(poll);
    clearInterval(tick);
  });
  const agoText = () => (updatedAt() ? `更新于 ${Math.max(0, Math.round((now() - updatedAt()) / 1000))} 秒前` : '');

  const resources = () => {
    const r = ov()!.resources;
    const loadPct = r.load !== null ? Math.min(100, Math.round((r.load / Math.max(1, r.cpus)) * 100)) : null;
    const memPct = r.mem_used_mb !== null && r.mem_total_mb ? Math.round((r.mem_used_mb / r.mem_total_mb) * 100) : null;
    return [
      { label: `负载（${r.cpus} 核）`, value: r.load !== null ? r.load.toFixed(2) : '不可用', pct: loadPct },
      {
        label: '内存',
        value:
          r.mem_used_mb !== null
            ? `${((r.mem_used_mb ?? 0) / 1024).toFixed(1)} / ${((r.mem_total_mb ?? 0) / 1024).toFixed(1)} GB`
            : '不可用',
        pct: memPct,
      },
      // 温度不是负载/占用率，不画进度条，只显示数值
      { label: '温度', value: r.temp_c !== null ? `${r.temp_c.toFixed(0)} °C` : '不可用', pct: null },
    ];
  };

  const services = () => {
    const o = ov()!;
    return [
      {
        name: 'hearth-server',
        meta: `Go 单体 · ${o.go_version} · 已运行 ${fmtUptime(o.uptime_seconds)}`,
        ok: true,
        state: 'running',
      },
      {
        name: `语音内核 · ${o.services.voice?.name ?? '?'}`,
        meta: o.services.voice?.url || '进程内嵌（/providers/<alias>/rtc）',
        ok: o.services.voice?.ok ?? false,
        state: o.services.voice?.ok ? 'running' : 'unreachable',
      },
      {
        name: `舞台内核 · ${o.services.stage?.name ?? '?'}`,
        meta: o.services.stage?.name === 'none' ? '未启用（投屏/摄像头不可用）' : (o.services.stage?.url ?? ''),
        ok: o.services.stage?.ok ?? false,
        state: o.services.stage?.ok ? 'running' : o.services.stage?.name === 'none' ? 'off' : 'unreachable',
      },
      {
        name: `推流入口 · ${o.services.ingest?.name ?? '?'}`,
        meta: o.services.ingest?.ok ? o.services.ingest.url : '未启用（在服务参数里补齐所选内核的配置）',
        ok: o.services.ingest?.ok ?? false,
        state: o.services.ingest?.ok ? 'running' : 'off',
      },
      { name: '数据库', meta: o.services.db?.url ?? '', ok: true, state: 'ok' },
    ];
  };

  return (
    <Show when={ov()} fallback={<Placeholder err={err()} />}>
      <div class="stat-cards">
        <div class="stat-card">
          <div class="s-label">注册用户</div>
          <div class="s-value">
            <span class="n">{ov()!.users}</span>
            <span class="u mono">个</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="s-label">在房人数</div>
          <div class="s-value">
            <span class="n">{ov()!.online}</span>
            <span class="u mono">人</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="s-label">频道</div>
          <div class="s-value">
            <span class="n">{ov()!.channels}</span>
            <span class="u mono">个</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="s-label">运行时长</div>
          <div class="s-value">
            <span class="n" style="font-size:20px">
              {fmtUptime(ov()!.uptime_seconds)}
            </span>
          </div>
        </div>
      </div>
      <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:16px">
        <div class="list-box">
          <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600;display:flex;align-items:baseline;gap:8px">
            <span>常驻组件</span>
            <span class="mono" style="font-size:10px;color:var(--text-3);margin-left:auto">{agoText()}</span>
          </div>
          <For each={services()}>
            {(sv) => (
              <div class="svc-row">
                <div class="dot" classList={{ down: !sv.ok }}></div>
                <div style="flex-grow:1;min-width:0">
                  <div class="s-name">{sv.name}</div>
                  <div class="s-meta mono">{sv.meta}</div>
                </div>
                <span class="mono" style={`font-size:11px;color:${sv.ok ? 'var(--sage)' : 'var(--red)'}`}>
                  {sv.state}
                </span>
              </div>
            )}
          </For>
        </div>
        <div class="list-box">
          <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600;display:flex;align-items:baseline;gap:8px">
            <span>宿主资源</span>
            <span class="mono" style="font-size:10px;color:var(--text-3);margin-left:auto">{agoText()}</span>
          </div>
          <div style="padding:16px 18px;display:flex;flex-direction:column;gap:15px">
            <For each={resources()}>
              {(res) => (
                <div>
                  <div style="display:flex;align-items:baseline;gap:8px">
                    <span style="font-size:12px;color:var(--text-1);flex-grow:1">{res.label}</span>
                    <span class="mono" style="font-size:11.5px">
                      {res.value}
                    </span>
                  </div>
                  <Show when={res.pct !== null}>
                    <div class="res-bar">
                      <div classList={{ hot: res.pct! > 80 }} style={`width:${Math.min(100, res.pct!)}%`}></div>
                    </div>
                  </Show>
                </div>
              )}
            </For>
            <div style="font-size:10.5px;color:var(--text-3)">资源读数来自 /proc 与 /sys，仅 Linux 宿主可用。</div>
          </div>
        </div>
      </div>
    </Show>
  );
}
