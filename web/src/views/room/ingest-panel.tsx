// 房间内的「OBS 推流」面板：把**这个频道**的完整 WHIP 地址与本人令牌摆出来，复制即用。
// 令牌是账号级的（每用户一把、不分频道），所以这里只读不改——重置与设备标签仍在设置「推流」页。
import { createSignal, Show } from 'solid-js';
import { getIngestToken } from '../../api';
import type { IngestTokenInfo } from '../../api';
import { copyText, el, icon, toast } from '../../ui';

export const IngestPanel = (p: {
  channel: string;
  channelId: () => number; // 0 = 频道列表还没回来（地址拼不出，先显示加载中）
  onClose: () => void;
  onOpenSettings: () => void;
}) => {
  // 令牌只在打开时拉一次（面板随 Show 建/毁，关掉再开就是重新拉）
  const [info, setInfo] = createSignal<IngestTokenInfo | null>(null);
  const [loadErr, setLoadErr] = createSignal('');
  const [reveal, setReveal] = createSignal(false);
  void getIngestToken()
    .then(setInfo)
    .catch((e) => setLoadErr((e as Error).message));

  const addr = () => {
    const i = info();
    const id = p.channelId();
    return i && id > 0 ? `${i.base}${id}` : '';
  };
  const token = () => info()?.token ?? '';
  const masked = () => {
    const t = token();
    return reveal() ? t : `${'•'.repeat(Math.max(0, t.length - 4))}${t.slice(-4)}`;
  };
  const copy = async (text: string, what: string) => {
    if (!text) return;
    if (await copyText(text)) toast(`已复制${what}`, 'ok', 1400);
    else toast('复制失败，请手动选中复制', 'bad');
  };

  return (
    <div class="ingest-scrim" onClick={p.onClose}>
      <div class="ingest-panel card" onClick={(ev) => ev.stopPropagation()}>
        <header class="ig-head">
          {el(icon('stream', 16, 'var(--ember)', 1.7))}
          <div class="ig-title">OBS 推流 · {p.channel}</div>
          <button type="button" id="ingest-close" class="hit btn btn-icon" aria-label="关闭" onClick={p.onClose}>
            {el(icon('close', 15, 'var(--text-1)', 1.8))}
          </button>
        </header>

        <Show when={loadErr()}>
          <div class="notice-bad">
            <span class="ig-note">{loadErr()}</span>
          </div>
        </Show>
        <Show when={info() && !info()!.enabled}>
          <div class="notice-bad">
            <span class="ig-note">
              当前舞台内核未启用或缺配置，推流入口不可用。地址和令牌照常可用，但现在推会被拒。
            </span>
          </div>
        </Show>

        <div class="ig-field">
          <div class="section-label">服务器（已含本频道）</div>
          <div class="copy-line">
            <span class="val mono">{addr() || '加载中…'}</span>
            <button
              type="button"
              id="ingest-copy-url"
              class="hit btn btn-sm"
              classList={{ disabled: !addr() }}
              onClick={() => void copy(addr(), '服务器地址')}
            >
              {el(icon('copy', 13))} 复制
            </button>
          </div>
        </div>

        <div class="ig-field">
          <div class="section-label">Bearer 令牌 · 全频道通用</div>
          <div class="copy-line">
            <span class="val mono">{token() ? masked() : '加载中…'}</span>
            <button
              type="button"
              id="ingest-reveal"
              class="hit ig-eye"
              aria-label={reveal() ? '隐藏令牌' : '显示令牌'}
              onClick={() => setReveal((v) => !v)}
            >
              {el(icon(reveal() ? 'eye' : 'eyeOff', 15, reveal() ? 'var(--ember)' : 'var(--text-2)', 1.6))}
            </button>
            <button
              type="button"
              id="ingest-copy-token"
              class="hit btn btn-sm"
              classList={{ disabled: !token() }}
              onClick={() => void copy(token(), '推流令牌')}
            >
              {el(icon('copy', 13))} 复制
            </button>
          </div>
        </div>

        <div class="ig-steps">
          <div class="ig-step">
            <span class="n">1</span>
            <span>
              OBS 设置 → 直播 → 服务选 <span class="mono ig-em">WHIP</span>
            </span>
          </div>
          <div class="ig-step">
            <span class="n">2</span>
            <span>服务器填上面那行地址</span>
          </div>
          <div class="ig-step">
            <span class="n">3</span>
            <span>Bearer Token 填上面那把令牌</span>
          </div>
        </div>
        <div class="ig-tip">
          编码器 H.264 / HEVC / AV1 均可，服务端直通不转码。ffmpeg 等不支持 Bearer 的工具用路径模式：地址末尾再拼一段{' '}
          <span class="mono ig-em">/令牌</span>。
        </div>

        <button type="button" id="ingest-go-settings" class="hit ig-link" onClick={p.onOpenSettings}>
          <span>令牌重置与设备标签在设置「推流」里</span>
          <span class="ig-arrow">→</span>
        </button>
      </div>
    </div>
  );
};
