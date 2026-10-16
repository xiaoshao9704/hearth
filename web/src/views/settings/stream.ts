// ---- 推流 ----
import { getIngestToken, resetIngestToken, setIngestTag } from '../../api';
import { copyText, esc, icon, toast } from '../../ui';

// 推流页只管账号级的东西：令牌查看/复制/重置、设备标签、OBS 填法。
// 「哪个频道的地址」属于频道级，归频道菜单的「OBS 推流地址…」面板，这里不再随打开位置变。
export function renderStream(body: HTMLElement) {
  let token = ''; // 推流令牌（每用户一把，不区分频道和设备）
  let tag = ''; // 已保存的设备标签（identity = {用户名}-{标签}）
  let enabled = true; // 推流入口是否可用（false 时地址照给，但推起来会被拒）
  let reveal = false;
  let confirming = false;
  let notice = '';

  body.innerHTML = '<div class="muted">加载推流信息…</div>';

  // 与服务端 ingestTagRe 一致
  const TAG_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;

  const paint = () => {
    if (!token) return; // 首屏等加载
    const masked = reveal ? token : `${'•'.repeat(Math.max(0, token.length - 4))}${token.slice(-4)}`;
    body.innerHTML = `
      <div class="pane-col pane-wide">
        ${
          enabled
            ? ''
            : `<div class="notice-bad"><span style="font-size:12px;line-height:1.55">推流进当前舞台内核：舞台内核未启用或缺配置时推流不可用。地址和令牌照常可用，但现在推会被拒。</span></div>`
        }
        <div class="hint-card" style="border-color:var(--line-soft)">
          ${icon('stream', 16, 'var(--ember)')}
          <div style="font-size:12px;line-height:1.7">服务器地址含频道，在频道菜单（房间顶栏的频道名）的「OBS 推流地址…」里复制；令牌全频道通用，就是下面这把。</div>
        </div>
        <div style="display:flex;flex-direction:column;gap:7px">
          <div class="section-label" style="letter-spacing:0.1em">推流令牌 · 全频道通用</div>
          <div class="copy-line">
            <span class="val mono">${esc(masked)}</span>
            <button class="hit mini-btn" data-act="reveal" style="width:30px;height:30px;border-radius:7px;display:flex;align-items:center;justify-content:center">${icon(reveal ? 'eye' : 'eyeOff', 15, reveal ? 'var(--ember)' : 'var(--text-2)', 1.6)}</button>
            <button class="hit btn btn-sm" data-copy="key">${icon('copy', 13)} 复制</button>
            <button class="hit btn btn-sm btn-danger" data-act="ask-reset">${icon('reset', 13, 'var(--red)')} 重置</button>
          </div>
        </div>
        ${
          confirming
            ? `<div class="notice-bad" style="gap:12px">
                 <span style="flex-grow:1;font-size:12px;line-height:1.55">重置推流令牌？旧令牌立即失效、进行中的推流会被掐断，OBS 里要重新填一次。</span>
                 <button class="hit btn btn-sm" data-act="cancel-reset">取消</button>
                 <button class="hit btn btn-sm btn-danger-solid" data-act="do-reset">确认重置</button>
               </div>`
            : ''
        }
        <div style="display:flex;flex-direction:column;gap:7px">
          <div class="section-label" style="letter-spacing:0.1em">设备标签</div>
          <div style="display:flex;align-items:center;gap:10px">
            <div class="field" style="flex-grow:1;height:40px;max-width:280px;background:var(--bg-2)"><input id="tag-input" value="${esc(tag)}" /></div>
            <button class="hit btn btn-primary disabled" id="tag-save" style="height:40px;padding:0 16px">保存</button>
          </div>
          <div id="tag-hint" style="font-size:11.5px;color:var(--text-2)">推流设备在房间里显示为「用户名-标签」；改完下次推流生效，正在推的流保持旧标签、不会断</div>
        </div>
        ${notice ? `<div class="notice-ok">${icon('check', 15, 'var(--sage)', 1.8)}<span>${esc(notice)}</span></div>` : ''}
        <div class="hint-card" style="border-color:var(--line-soft)">
          ${icon('check', 16, 'var(--sage)')}
          <div style="display:flex;flex-direction:column;gap:6px">
            <div style="font-size:12.5px;font-weight:600;color:var(--text-0)">OBS 里怎么填</div>
            <div style="font-size:12px;line-height:1.7">设置 → 直播 → 服务选 <span class="mono" style="color:var(--text-1)">WHIP</span>，服务器填频道的完整地址（频道菜单的「OBS 推流地址…」里复制，换频道换地址、令牌不变），Bearer Token 填推流令牌。编码器 H.264 / HEVC / AV1 均可，服务端直通不转码，<span style="color:var(--sage)">2K / 4K / 120fps 原样透传</span>。ffmpeg 等不支持 Bearer 的工具用路径模式：服务器地址末尾再拼一段 <span class="mono" style="color:var(--text-1)">/令牌</span>。</div>
          </div>
        </div>
      </div>`;

    body.querySelectorAll<HTMLButtonElement>('[data-copy]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        if (!token) return;
        if (await copyText(token)) toast('已复制', 'ok', 1400);
      });
    });
    body.querySelector('[data-act="reveal"]')?.addEventListener('click', () => {
      reveal = !reveal;
      paint();
    });
    body.querySelector('[data-act="ask-reset"]')?.addEventListener('click', () => {
      confirming = true;
      notice = '';
      paint();
    });
    body.querySelector('[data-act="cancel-reset"]')?.addEventListener('click', () => {
      confirming = false;
      paint();
    });
    body.querySelector('[data-act="do-reset"]')?.addEventListener('click', () => {
      confirming = false;
      void (async () => {
        try {
          const info = await resetIngestToken();
          token = info.token;
          tag = info.tag;
          enabled = info.enabled;
          reveal = false;
          notice = '已生成新令牌，旧令牌名下的推流已掐断。';
          setTimeout(() => {
            notice = '';
            paint();
          }, 2600);
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
        paint();
      })();
    });

    const tagInput = body.querySelector<HTMLInputElement>('#tag-input')!;
    const tagSave = body.querySelector<HTMLButtonElement>('#tag-save')!;
    const tagHint = body.querySelector<HTMLDivElement>('#tag-hint')!;
    const syncTag = () => {
      const v = tagInput.value.trim();
      const valid = TAG_RE.test(v);
      tagSave.classList.toggle('disabled', !valid || v === tag);
      if (v && !valid) {
        tagHint.textContent = '标签仅限 1-32 位小写字母、数字、-，且以字母或数字开头';
        tagHint.style.color = 'var(--red-text)';
      } else {
        tagHint.textContent = '推流设备在房间里显示为「用户名-标签」；改完下次推流生效，正在推的流保持旧标签、不会断';
        tagHint.style.color = 'var(--text-2)';
      }
    };
    tagInput.addEventListener('input', syncTag);
    tagSave.addEventListener('click', async () => {
      const v = tagInput.value.trim();
      if (tagSave.classList.contains('disabled')) return;
      try {
        const info = await setIngestTag(v);
        tag = info.tag;
        toast(`设备标签已改成「${tag}」，下次推流生效。`, 'ok');
        paint();
      } catch (err) {
        toast((err as Error).message, 'bad');
      }
    });
  };

  void (async () => {
    try {
      const info = await getIngestToken();
      token = info.token;
      tag = info.tag;
      enabled = info.enabled;
    } catch (err) {
      toast((err as Error).message, 'bad');
      return;
    }
    paint();
  })();
}
