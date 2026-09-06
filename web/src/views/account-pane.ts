// 「账户」pane 的登录会话卡片：列出这个账号当前有效的登录，并能把某台设备下线。
// 与「我的设备」pane 的区别：那里是进房时记录的设备档案（移除不影响登录），
// 这里是真的登录凭证——下线即刻生效，那台设备下一个请求就 401。
import { clearSession, deleteMySession, listMySessions } from '../api';
import type { SessionRecord } from '../api';
import { confirmDialog, esc, icon, timeAgo, toast } from '../ui';

// uaLabel 把 UA 原文压成一行人看得懂的设备名。UA 是不可靠的自述字符串，
// 认不出来就退回原文截断——目的只是让人能认出"哪一台"，不做设备指纹。
function uaLabel(ua: string): string {
  if (!ua) return '未知设备';
  const os = /iPhone/i.test(ua)
    ? 'iPhone'
    : /iPad/i.test(ua)
      ? 'iPad'
      : /Android/i.test(ua)
        ? 'Android'
        : /Mac OS X|Macintosh/i.test(ua)
          ? 'macOS'
          : /Windows/i.test(ua)
            ? 'Windows'
            : /Linux/i.test(ua)
              ? 'Linux'
              : '';
  const browser = /Edg\//i.test(ua)
    ? 'Edge'
    : /OPR\//i.test(ua)
      ? 'Opera'
      : /Firefox\//i.test(ua)
        ? 'Firefox'
        : /Chrome\//i.test(ua)
          ? 'Chrome'
          : /Safari\//i.test(ua)
            ? 'Safari'
            : '';
  const parts = [browser, os].filter(Boolean);
  return parts.length > 0 ? parts.join(' · ') : ua.slice(0, 48);
}

function isMobileUA(ua: string): boolean {
  return /iPhone|iPad|Android/i.test(ua);
}

function rowHtml(s: SessionRecord): string {
  const when = s.last_seen ?? s.created_at;
  const meta = [when ? `活跃于 ${timeAgo(when)}` : '活跃时间未知', s.created_at ? `登录于 ${timeAgo(s.created_at)}` : '']
    .filter(Boolean)
    .join(' · ');
  return `
    <div class="list-row">
      <div class="sess-icon${s.current ? ' on' : ''}">
        ${icon(isMobileUA(s.user_agent) ? 'phone' : 'device', 17, s.current ? 'var(--ember)' : 'var(--text-1)', 1.6)}
      </div>
      <div style="flex-grow:1;min-width:0">
        <div style="display:flex;align-items:center;gap:8px;min-width:0">
          <span class="sess-name">${esc(uaLabel(s.user_agent))}</span>
          ${s.current ? '<span class="tag tag-sage" style="flex-shrink:0">本次登录</span>' : ''}
        </div>
        <div class="sess-meta mono">${esc(meta)}</div>
      </div>
      <button class="hit btn btn-sm${s.current ? '' : ' btn-danger'}" data-sess="${esc(s.id)}" style="flex-shrink:0">
        ${s.current ? '退出登录' : '下线'}
      </button>
    </div>`;
}

// renderSessions 把会话卡片渲染进 host（由「账户」pane 挂在改密卡片下方）。
export function renderSessions(host: HTMLElement) {
  host.innerHTML = `<div class="card"><div class="muted">加载登录会话…</div></div>`;

  async function paint() {
    let sessions: SessionRecord[];
    try {
      sessions = await listMySessions();
    } catch (err) {
      host.innerHTML = `<div class="card"><div class="error-text">${esc((err as Error).message)}</div></div>`;
      return;
    }
    host.innerHTML = `
      <div class="card">
        <div style="font-size:13.5px;font-weight:600">登录会话</div>
        <div style="font-size:11.5px;color:var(--text-2);margin-top:4px">这个账号当前有效的登录，下线即刻生效</div>
        <div class="list-box" style="margin-top:13px">
          ${sessions.map(rowHtml).join('') || '<div class="table-empty">没有有效会话。</div>'}
        </div>
      </div>`;

    host.querySelectorAll<HTMLButtonElement>('[data-sess]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const id = btn.dataset.sess!;
        const self = sessions.some((s) => s.id === id && s.current);
        const ok = await confirmDialog({
          title: self ? '退出这台设备？' : '让那台设备下线？',
          body: self ? '当前页面会回到登录页。' : '那台设备下一次请求就会被要求重新登录。',
          danger: true,
          confirmText: self ? '退出登录' : '下线',
        });
        if (!ok) return;
        try {
          await deleteMySession(id);
        } catch (err) {
          toast((err as Error).message, 'bad');
          return;
        }
        if (self) {
          clearSession();
          location.hash = '#/login';
          location.reload();
          return;
        }
        toast('那台设备已下线。', 'ok');
        void paint();
      });
    });
  }

  void paint();
}
