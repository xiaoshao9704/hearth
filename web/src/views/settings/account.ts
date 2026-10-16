// ---- 账户 ----
import {
  claimAccount,
  clearSession,
  getUser,
  guestTimeLeft,
  isGuest,
  logout,
  updatePassword,
  updateUsername,
} from '../../api';
import { renderPasskeys, renderSessions } from '../account-pane';
import { avatarHtml, esc, icon, pwBarsHtml, pwScore, toast } from '../../ui';

export function renderAccount(body: HTMLElement, close: () => void) {
  const user = getUser();
  // 访客没有密码可改，取而代之的是「注册保留身份」（转正：user_id 不变）；
  // 显隐只看服务端下发的 can_claim：站点没开 guest_claim 就连表单都不渲染（服务端也会 403）
  const guest = isGuest(user);
  const claimable = !!user?.can_claim;
  const guestNoClaimCard = `
      <div class="card">
        <div style="font-size:13.5px;font-weight:600">访客身份</div>
        <div style="font-size:11.5px;line-height:1.6;color:var(--text-2);margin-top:4px;text-wrap:pretty">
          本站未开放访客转正，访客身份到期即失效。
        </div>
      </div>`;
  const guestClaimCard = `
      <div class="card" style="border-color:var(--ember-line)">
        <div style="font-size:13.5px;font-weight:600">注册保留身份</div>
        <div style="font-size:11.5px;line-height:1.6;color:var(--text-2);margin-top:4px;text-wrap:pretty">
          你现在是访客，${esc(guestTimeLeft(user))}，到期账号会被清理。设一个密码就转成正式账号：
          user_id 不变，聊天记录、频道里的位置都跟着留下，也不再绑这台浏览器。
        </div>
        <div style="display:flex;flex-direction:column;gap:11px;margin-top:13px">
          <div>
            <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">用户名</div>
            <div class="field" style="height:42px;background:var(--bg-2)"><input id="cl-user" value="${esc(user?.username ?? '')}" autocapitalize="off" autocomplete="username" /></div>
          </div>
          <div style="display:flex;gap:11px;flex-wrap:wrap">
            <div style="flex-grow:1;min-width:180px">
              <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">密码</div>
              <div class="field" style="height:42px;background:var(--bg-2)"><input id="cl-new" type="password" placeholder="至少 8 位" autocomplete="new-password" /></div>
            </div>
            <div style="flex-grow:1;min-width:180px">
              <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">确认密码</div>
              <div class="field" style="height:42px;background:var(--bg-2)"><input id="cl-conf" type="password" placeholder="再输一次" autocomplete="new-password" /></div>
            </div>
          </div>
          <div class="pw-bars" id="cl-pw-bars">${pwBarsHtml(0)}</div>
          <div style="display:flex;align-items:center;gap:12px">
            <div id="cl-hint" style="font-size:11.5px;color:var(--text-2)"></div>
            <div class="spacer"></div>
            <button class="hit btn btn-primary disabled" id="cl-save">注册保留身份</button>
          </div>
        </div>
      </div>`;
  body.innerHTML = `
    <div class="pane-col pane-narrow">
      <div class="card">
        <div style="display:flex;align-items:center;gap:14px">
          ${avatarHtml(user?.username ?? '?', 'avatar avatar-lg')}
          <div style="flex-grow:1;min-width:0">
            <div style="font-size:14px;font-weight:600" id="acc-name">${esc(user?.username ?? '')}</div>
            <div class="mono" style="font-size:11px;color:var(--text-2);margin-top:3px">user_id · ${user?.id ?? '?'}</div>
          </div>
          ${guest ? `<span class="tag" style="font-size:10.5px;padding:4px 9px">访客 · ${esc(guestTimeLeft(user))}</span>` : ''}
          ${user?.is_admin ? '<span class="tag tag-ember" style="font-size:10.5px;padding:4px 9px">管理员</span>' : ''}
        </div>
        <div style="margin-top:13px;padding-top:13px;border-top:1px solid var(--line-soft);font-size:11.5px;line-height:1.65;color:var(--text-2);text-wrap:pretty">系统内部一律按 <span class="mono" style="color:var(--text-1)">user_id</span> 认人：改用户名不会动它，历史消息、设备档案、推流令牌都还挂在同一个 id 上。</div>
      </div>

      <div class="card">
        <div style="font-size:13.5px;font-weight:600">用户名</div>
        <div style="font-size:11.5px;color:var(--text-2);margin-top:4px">别人在频道里看到的名字，也是你登录时用的</div>
        <div style="display:flex;align-items:center;gap:10px;margin-top:13px">
          <div class="field" style="flex-grow:1;height:42px"><input id="name-input" value="${esc(user?.username ?? '')}" /></div>
          <button class="hit btn btn-primary disabled" id="name-save" style="height:42px;padding:0 18px">保存</button>
        </div>
        <div id="name-hint" style="margin-top:9px;font-size:11.5px;color:var(--text-2)">和当前用户名相同</div>
      </div>

      ${guest ? (claimable ? guestClaimCard : guestNoClaimCard) : `<div class="card">
        <div style="font-size:13.5px;font-weight:600">修改密码</div>
        <div style="font-size:11.5px;color:var(--text-2);margin-top:4px">改完其他设备上的会话会全部退出，需要重新登录</div>
        <div style="display:flex;flex-direction:column;gap:11px;margin-top:13px">
          <div>
            <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">当前密码</div>
            <div class="field" style="height:42px;background:var(--bg-2)"><input id="pw-cur" type="password" placeholder="验证是你本人" autocomplete="current-password" /></div>
          </div>
          <div style="display:flex;gap:11px;flex-wrap:wrap">
            <div style="flex-grow:1;min-width:180px">
              <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">新密码</div>
              <div class="field" style="height:42px;background:var(--bg-2)"><input id="pw-new" type="password" placeholder="至少 8 位" autocomplete="new-password" /></div>
            </div>
            <div style="flex-grow:1;min-width:180px">
              <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">确认新密码</div>
              <div class="field" style="height:42px;background:var(--bg-2)"><input id="pw-conf" type="password" placeholder="再输一次" autocomplete="new-password" /></div>
            </div>
          </div>
          <div class="pw-bars" id="acc-pw-bars">${pwBarsHtml(0)}</div>
          <div style="display:flex;align-items:center;gap:12px">
            <div id="pw-hint" style="font-size:11.5px;color:var(--text-2)"></div>
            <div class="spacer"></div>
            <button class="hit btn btn-primary disabled" id="pw-save">修改密码</button>
          </div>
        </div>
      </div>`}

      <div id="acc-sessions"></div>

      ${guest ? '' : '<div id="acc-passkeys"></div>'}

      <button class="hit card" id="acc-logout" style="display:flex;align-items:center;gap:10px;padding:14px 18px;border-color:var(--red-line);text-align:left;width:100%">
        ${icon('leave', 16, 'var(--red)')}
        <div style="flex-grow:1">
          <div style="font-size:13px;font-weight:600;color:var(--red-text)">退出登录</div>
          <div style="font-size:11.5px;color:var(--text-2);margin-top:3px">只退这台设备，其他设备不受影响</div>
        </div>
      </button>
    </div>
  `;

  const nameInput = body.querySelector<HTMLInputElement>('#name-input')!;
  const nameSave = body.querySelector<HTMLButtonElement>('#name-save')!;
  const nameHint = body.querySelector<HTMLDivElement>('#name-hint')!;
  const NAME_RE = /^[a-zA-Z0-9_-]{2,32}$/;

  function syncName() {
    const v = nameInput.value.trim();
    const valid = NAME_RE.test(v);
    const changed = v !== (getUser()?.username ?? '');
    nameSave.classList.toggle('disabled', !valid || !changed);
    nameHint.textContent = !valid ? '用户名需 2–32 位字母数字 - _' : changed ? '改完别人看到的名字会立刻变' : '和当前用户名相同';
    nameHint.style.color = valid ? 'var(--text-2)' : 'var(--red-text)';
  }
  nameInput.addEventListener('input', syncName);
  nameSave.addEventListener('click', async () => {
    const v = nameInput.value.trim();
    if (nameSave.classList.contains('disabled')) return;
    try {
      const u = await updateUsername(v);
      body.querySelector('#acc-name')!.textContent = u.username;
      toast(`用户名已改成「${u.username}」，user_id 没变。`, 'ok');
      syncName();
    } catch (err) {
      toast((err as Error).message, 'bad');
    }
  });

  if (guest) {
    if (claimable) wireClaim(body, close);
  } else wirePassword(body);

  renderSessions(body.querySelector<HTMLElement>('#acc-sessions')!);
  // 访客看不到通行密钥卡片（服务端也拒，见 api.requirePasskeyAccount）
  if (!guest) renderPasskeys(body.querySelector<HTMLElement>('#acc-passkeys')!);

  body.querySelector('#acc-logout')!.addEventListener('click', async () => {
    try {
      await logout();
    } catch {
      clearSession();
    }
    close();
    location.hash = '#/login';
  });
}

// wireClaim 访客转正表单：成功后当前会话继续有效（服务端解除设备绑定），
// 整个 pane 重画一次——身份变了，用户名卡片、访客标签、下面的会话列表都要跟着变。
function wireClaim(body: HTMLElement, close: () => void) {
  const userInput = body.querySelector<HTMLInputElement>('#cl-user')!;
  const pwNew = body.querySelector<HTMLInputElement>('#cl-new')!;
  const pwConf = body.querySelector<HTMLInputElement>('#cl-conf')!;
  const save = body.querySelector<HTMLButtonElement>('#cl-save')!;
  const hintEl = body.querySelector<HTMLDivElement>('#cl-hint')!;
  const NAME_RE = /^[a-zA-Z0-9_-]{2,32}$/;
  let busy = false;

  function sync() {
    body.querySelector('#cl-pw-bars')!.innerHTML = pwBarsHtml(pwScore(pwNew.value));
    const nameOk = NAME_RE.test(userInput.value.trim());
    const ready = nameOk && pwNew.value.length >= 8 && pwNew.value === pwConf.value;
    let hint = '';
    let tone = 'var(--text-2)';
    if (userInput.value && !nameOk) {
      hint = '用户名需 2–32 位字母数字 - _';
      tone = 'var(--red-text)';
    } else if (pwNew.value && pwNew.value.length < 8) {
      hint = `密码还差 ${8 - pwNew.value.length} 位`;
      tone = 'var(--red-text)';
    } else if (pwConf.value && pwNew.value !== pwConf.value) {
      hint = '两次输入不一样';
      tone = 'var(--red-text)';
    } else if (ready) {
      hint = '转正后不再有过期时间';
      tone = 'var(--sage)';
    }
    hintEl.textContent = hint;
    hintEl.style.color = tone;
    save.classList.toggle('disabled', !ready || busy);
  }
  [userInput, pwNew, pwConf].forEach((el) => el.addEventListener('input', sync));

  save.addEventListener('click', async () => {
    if (save.classList.contains('disabled') || busy) return;
    busy = true;
    save.classList.add('loading');
    try {
      const u = await claimAccount(userInput.value.trim(), pwNew.value);
      toast(`已注册为「${u.username}」，user_id 没变，身份保留下来了。`, 'ok');
      renderAccount(body, close); // 身份变了，整个 pane 重画
      return;
    } catch (err) {
      toast((err as Error).message, 'bad');
    } finally {
      busy = false;
      save.classList.remove('loading');
      sync();
    }
  });
  sync();
}

function wirePassword(body: HTMLElement) {
  const pwCur = body.querySelector<HTMLInputElement>('#pw-cur')!;
  const pwNew = body.querySelector<HTMLInputElement>('#pw-new')!;
  const pwConf = body.querySelector<HTMLInputElement>('#pw-conf')!;
  const pwSave = body.querySelector<HTMLButtonElement>('#pw-save')!;
  const pwHint = body.querySelector<HTMLDivElement>('#pw-hint')!;

  function syncPw() {
    body.querySelector('#acc-pw-bars')!.innerHTML = pwBarsHtml(pwScore(pwNew.value));
    const ready = pwCur.value.length > 0 && pwNew.value.length >= 8 && pwNew.value === pwConf.value;
    let hint = '';
    let tone = 'var(--text-2)';
    if (pwNew.value && pwNew.value.length < 8) {
      hint = `新密码还差 ${8 - pwNew.value.length} 位`;
      tone = 'var(--red-text)';
    } else if (pwConf.value && pwNew.value !== pwConf.value) {
      hint = '两次输入不一样';
      tone = 'var(--red-text)';
    } else if (pwNew.value && !pwCur.value) {
      hint = '还要填当前密码';
    } else if (ready) {
      hint = '可以改了';
      tone = 'var(--sage)';
    }
    pwHint.textContent = hint;
    pwHint.style.color = tone;
    pwSave.classList.toggle('disabled', !ready);
  }
  [pwCur, pwNew, pwConf].forEach((el) => el.addEventListener('input', syncPw));
  pwSave.addEventListener('click', async () => {
    if (pwSave.classList.contains('disabled')) return;
    try {
      await updatePassword(pwCur.value, pwNew.value);
      pwCur.value = pwNew.value = pwConf.value = '';
      syncPw();
      toast('密码已更新，其他设备上的会话已全部退出。', 'ok');
      renderSessions(body.querySelector<HTMLElement>('#acc-sessions')!); // 会话列表跟着变，重画一次
    } catch (err) {
      toast((err as Error).message, 'bad');
    }
  });
}
