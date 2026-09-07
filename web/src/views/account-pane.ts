// 「账户」pane 的登录会话卡片：列出这个账号当前有效的登录，并能把某台设备下线。
// 与「我的设备」pane 的区别：那里是进房时记录的设备档案（移除不影响登录），
// 这里是真的登录凭证——下线即刻生效，那台设备下一个请求就 401。
import { clearSession, deleteMySession, listMySessions } from '../api';
import type { SessionRecord } from '../api';
import { deletePasskey, isSupported, listPasskeys, passkeyErrorText, registerPasskey, renamePasskey } from '../passkey';
import type { PasskeyRecord } from '../passkey';
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

// ---- 通行密钥卡片 ----
// 与会话卡片的区别：会话是「已经登进来的设备」，通行密钥是「以后能登进来的凭钥」。
// 不做「最后一枚不能删」的约束——密码始终保留，删空了照样能用密码进。

function passkeyRowHtml(p: PasskeyRecord, editing: boolean): string {
  const meta = [`添加于 ${timeAgo(p.created_at)}`, p.last_used_at ? `最近用于 ${timeAgo(p.last_used_at)}` : '还没用过'].join(' · ');
  const name = editing
    ? `<div class="field pk-edit"><input data-pk-name="${p.id}" value="${esc(p.name)}" maxlength="40" /></div>`
    : `<span class="sess-name">${esc(p.name)}</span>`;
  return `
    <div class="list-row">
      <div class="sess-icon">${icon('key', 17, 'var(--text-1)', 1.6)}</div>
      <div style="flex-grow:1;min-width:0">
        <div style="display:flex;align-items:center;gap:8px;min-width:0">
          ${name}
          ${p.backup_state ? '<span class="tag tag-sage" style="flex-shrink:0">已同步备份</span>' : ''}
        </div>
        <div class="sess-meta mono">${esc(meta)}</div>
      </div>
      ${
        editing
          ? `<button class="hit btn btn-sm btn-primary" data-pk-save="${p.id}" style="flex-shrink:0">保存</button>
             <button class="hit btn btn-sm" data-pk-cancel="${p.id}" style="flex-shrink:0">取消</button>`
          : `<button class="hit btn btn-sm" data-pk-rename="${p.id}" style="flex-shrink:0">改名</button>
             <button class="hit btn btn-sm btn-danger" data-pk-del="${p.id}" style="flex-shrink:0">删除</button>`
      }
    </div>`;
}

// renderPasskeys 把通行密钥卡片渲染进 host（由「账户」pane 挂在会话卡片下方）。
// 访客不该看到这张卡，由调用方决定不挂。
export function renderPasskeys(host: HTMLElement) {
  const head = `
    <div style="font-size:13.5px;font-weight:600">通行密钥</div>
    <div style="font-size:11.5px;line-height:1.6;color:var(--text-2);margin-top:4px;text-wrap:pretty">用指纹、面容或设备密码登录，不用输密码。私钥留在设备里（或跟着系统账号同步），服务器只存公钥。</div>`;

  if (!isSupported()) {
    host.innerHTML = `<div class="card">${head}
      <div class="table-empty" style="margin-top:13px">这个浏览器不支持通行密钥，换新版 Safari / Chrome / Edge 再来添加。已添加的凭钥不受影响。</div>
    </div>`;
    return;
  }

  host.innerHTML = `<div class="card">${head}<div class="muted" style="margin-top:13px">加载通行密钥…</div></div>`;
  let editing = 0; // 正在改名的那一行 id，0 = 没有
  let busy = false;

  async function paint() {
    let keys: PasskeyRecord[];
    try {
      keys = await listPasskeys();
    } catch (err) {
      host.innerHTML = `<div class="card">${head}<div class="error-text" style="margin-top:13px">${esc((err as Error).message)}</div></div>`;
      return;
    }
    host.innerHTML = `
      <div class="card">
        ${head}
        <div class="list-box" style="margin-top:13px">
          ${keys.map((p) => passkeyRowHtml(p, p.id === editing)).join('') || '<div class="table-empty">还没有通行密钥。</div>'}
        </div>
        <div style="display:flex;align-items:center;gap:12px;margin-top:13px;flex-wrap:wrap">
          <div style="font-size:11.5px;color:var(--text-2)">每台设备加一枚；用手机扫码添加的那枚会跟着手机的系统账号同步。</div>
          <div class="spacer"></div>
          <button class="hit btn btn-primary" id="pk-add">添加通行密钥</button>
        </div>
      </div>`;

    const addBtn = host.querySelector<HTMLButtonElement>('#pk-add')!;
    addBtn.addEventListener('click', async () => {
      if (busy) return;
      busy = true;
      addBtn.classList.add('loading');
      try {
        const p = await registerPasskey();
        toast(`已添加通行密钥「${p.name}」，下次登录一键就进。`, 'ok');
        editing = 0;
        busy = false;
        void paint();
        return;
      } catch (err) {
        const msg = passkeyErrorText(err); // 用户取消返回空串，不弹 toast
        if (msg) toast(msg, 'bad');
      }
      busy = false;
      addBtn.classList.remove('loading');
    });

    host.querySelectorAll<HTMLButtonElement>('[data-pk-rename]').forEach((btn) => {
      btn.addEventListener('click', () => {
        editing = Number(btn.dataset.pkRename);
        void paint();
      });
    });
    host.querySelectorAll<HTMLButtonElement>('[data-pk-cancel]').forEach((btn) => {
      btn.addEventListener('click', () => {
        editing = 0;
        void paint();
      });
    });

    const save = async (id: number) => {
      const input = host.querySelector<HTMLInputElement>(`[data-pk-name="${id}"]`);
      const name = input?.value.trim() ?? '';
      if (!name) {
        toast('名字不能为空', 'bad');
        return;
      }
      try {
        await renamePasskey(id, name);
      } catch (err) {
        toast((err as Error).message, 'bad');
        return;
      }
      editing = 0;
      toast('名字已改。', 'ok');
      void paint();
    };
    host.querySelectorAll<HTMLButtonElement>('[data-pk-save]').forEach((btn) => {
      btn.addEventListener('click', () => void save(Number(btn.dataset.pkSave)));
    });
    host.querySelectorAll<HTMLInputElement>('[data-pk-name]').forEach((input) => {
      input.addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter') void save(Number(input.dataset.pkName));
        if (ev.key === 'Escape') {
          editing = 0;
          void paint();
        }
      });
      input.focus();
      input.select();
    });

    host.querySelectorAll<HTMLButtonElement>('[data-pk-del]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const id = Number(btn.dataset.pkDel);
        const p = keys.find((k) => k.id === id);
        const ok = await confirmDialog({
          title: '删除这枚通行密钥？',
          body: `「${p?.name ?? ''}」删掉后就不能再用它登录了，密码登录不受影响。想再用得重新添加一枚。`,
          danger: true,
          confirmText: '删除',
        });
        if (!ok) return;
        try {
          await deletePasskey(id);
        } catch (err) {
          toast((err as Error).message, 'bad');
          return;
        }
        toast('通行密钥已删除。', 'ok');
        editing = 0;
        void paint();
      });
    });
  }

  void paint();
}
