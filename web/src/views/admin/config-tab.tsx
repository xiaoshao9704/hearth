// 管理后台「服务参数」tab：组件地址、服务实例与注册策略。
import { createEffect, createMemo, createSignal, For, Show } from 'solid-js';
import { adminGetConfig, adminGetPolicy, adminOverview, adminSetConfig, adminSetPolicy } from '../../api';
import type { AdminOverview, ConfigItem } from '../../api';
import { toast } from '../../ui';
import { ProvidersCard } from './providers-card';
import { TlsCard } from './tls-card';
import { Placeholder } from './shared';

// 枚举值的人话标签（值本身仍以英文存库）；选择器可选项是实例 alias，未知名直接显示原值
const KERNEL_LABELS: Record<string, string> = {
  livekit: 'LiveKit',
  lkembed: 'LiveKit（进程内）',
  none: '关闭',
  auto: '自动',
  off: '关闭',
};
const GROUP_META: Record<string, [string, string]> = {
  core: ['内核选择', '语音 / 舞台（投屏）分别选服务实例；OBS 推流一律进当前舞台内核'],
  livekit: ['LiveKit', '信令与令牌签发'],
  stage: ['lkembed（舞台）', '舞台选 lkembed 时才启动，信令只监听回环'],
  network: ['网络', '向默认网关申请端口映射，仅 host 网络或裸机可用'],
  chat: ['聊天', '消息经内核数据通道扇出，hearth 只落库卡片、不经手文件字节'],
  admin: ['管理', '审计流水的保留策略'],
};
const POLICIES = [
  { id: 'closed', label: '关闭注册', desc: '只能用 CLI 在服务器上开通' },
  { id: 'invite', label: '邀请制', desc: '管理员发链接，有效期内可自助注册' },
  { id: 'open', label: '开放注册', desc: '任何人都能注册——公网上不建议' },
];
export function ConfigTab(props: { onDirty: (groups: string[]) => void }) {
  const [ov, setOv] = createSignal<AdminOverview>();
  const [items, setItems] = createSignal<ConfigItem[]>([]);
  const [policy, setPolicy] = createSignal('');
  const [defaultRole, setDefaultRole] = createSignal(''); // 注册产出默认档（user/power）
  const [policyBusy, setPolicyBusy] = createSignal(''); // 正在切换的策略/默认档标识
  const [err, setErr] = createSignal('');
  // 输入草稿：只存被改过的键，未改的键取 ConfigItem.value（= 已保存值），两者比较即脏标记
  const [draft, setDraft] = createSignal<Record<string, string>>({});
  const [saving, setSaving] = createSignal(''); // 正在保存的配置组名，空串=空闲
  const [provDirty, setProvDirty] = createSignal(false);

  void (async () => {
    try {
      const [o, its, pol] = await Promise.all([adminOverview(), adminGetConfig(), adminGetPolicy()]);
      setItems(its);
      setPolicy(pol.policy);
      setDefaultRole(pol.default_role);
      setOv(o);
    } catch (e) {
      setErr((e as Error).message);
    }
  })();

  // 策略与默认档是同一份设置，任一变都两项一起提交
  const savePolicy = async (nextPolicy: string, nextRole: string, busyKey: string) => {
    if (policyBusy()) return;
    setPolicyBusy(busyKey);
    try {
      const r = await adminSetPolicy(nextPolicy, nextRole);
      setPolicy(r.policy);
      setDefaultRole(r.default_role);
      toast('注册策略已更新', 'ok');
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setPolicyBusy('');
    }
  };

  const allGroups = createMemo(() => [...new Set(items().map((it) => it.group))]);
  // server 组（四个 tls_* 键）不进这套通用卡片：TlsCard 自己渲染，
  // 但保存仍复用这里的 draft/saveGroup（见下方 <TlsCard>），脏标记也仍算进未保存改动提示
  const groups = createMemo(() => allGroups().filter((g) => g !== 'server'));
  const groupItems = (g: string) => items().filter((it) => it.group === g);
  const valOf = (it: ConfigItem) => draft()[it.name] ?? it.value;
  const setVal = (it: ConfigItem, v: string) => setDraft((d) => ({ ...d, [it.name]: v }));
  const dirtyOf = (g: string) => groupItems(g).some((it) => !it.locked && valOf(it) !== it.value);
  const dirty = createMemo(() => allGroups().filter(dirtyOf));

  createEffect(() => props.onDirty(provDirty() ? [...dirty(), '__provider'] : dirty()));

  // 该组存过之后把服务端值拉回来、清掉草稿。上传证书接口自己会把 tls_cert_source 落成
  // upload，不经这里的 saveGroup，所以它也要能单独调这一步，否则草稿会一直是「未保存」。
  const syncGroup = async (g: string) => {
    const fresh = await adminGetConfig();
    setItems(fresh);
    setDraft((d) => {
      const next = { ...d };
      for (const it of fresh) if (it.group === g) delete next[it.name];
      return next;
    });
  };

  const saveGroup = async (g: string) => {
    if (saving()) return;
    const values: Record<string, string> = {};
    for (const it of groupItems(g)) {
      if (it.locked) continue;
      const v = valOf(it);
      if (it.options?.length) {
        values[it.name] = v;
        continue;
      }
      // 密钥留空表示保持不变，不提交
      if (it.secret && it.set && v.trim() === '') continue;
      values[it.name] = v.trim();
    }
    setSaving(g);
    try {
      await adminSetConfig(values);
      toast('已保存并生效', 'ok');
      await syncGroup(g);
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setSaving('');
    }
  };

  // 依赖服务配置卡片：环境变量固定的只读展示；未固定的可编辑，保存落库即时生效
  const GroupCard = (gp: { group: string }) => {
    const list = () => groupItems(gp.group);
    const title = () => GROUP_META[gp.group]?.[0] ?? gp.group;
    const sub = () => GROUP_META[gp.group]?.[1] ?? '';
    return (
      <div class="card">
        <div style="display:flex;align-items:baseline;gap:9px;margin-bottom:4px">
          <div style="font-size:13px;font-weight:600">{title()}</div>
          <div style="font-size:11px;color:var(--text-2);flex-grow:1">{sub()}</div>
          <Show when={dirtyOf(gp.group)}>
            <span class="tag tag-ember">未保存</span>
          </Show>
        </div>
        <div style="display:flex;flex-direction:column;gap:11px;margin-top:11px">
          <For each={list()}>
            {(it) => (
              <div>
                <div style="display:flex;align-items:baseline;gap:8px;margin-bottom:6px">
                  <span style="font-size:11px;color:var(--text-2)">{it.label}</span>
                  <span class="mono" style="font-size:10px;color:var(--text-3)">
                    {it.env}
                  </span>
                  <Show when={it.locked}>
                    <span class="tag" title="部署侧 .env / compose 里改，重启生效">
                      环境变量固定
                    </span>
                  </Show>
                </div>
                {it.locked ? (
                  <div
                    class="mono"
                    style="padding:9px 12px;border-radius:8px;background:var(--bg-2);border:1px solid var(--line);font-size:12px;color:var(--text-1);overflow:hidden;text-overflow:ellipsis;white-space:nowrap"
                  >
                    {it.secret ? '••••••••' : it.value || '（空）'}
                  </div>
                ) : it.options?.length ? (
                  <div class="seg-group" style="background:var(--bg-2)">
                    <For each={it.options}>
                      {(opt) => (
                        <button class="hit seg" classList={{ on: valOf(it) === opt }} onClick={() => setVal(it, opt)}>
                          {KERNEL_LABELS[opt] ?? opt}
                        </button>
                      )}
                    </For>
                  </div>
                ) : (
                  <div class="field" style="height:38px;background:var(--bg-2)">
                    <input
                      class="mono"
                      style="font-size:12px"
                      value={valOf(it)}
                      placeholder={it.secret && it.set ? '已设置（输入新值覆盖）' : it.hint}
                      autocomplete={it.secret ? 'off' : undefined}
                      onInput={(ev) => setVal(it, ev.currentTarget.value)}
                    />
                  </div>
                )}
              </div>
            )}
          </For>
        </div>
        <Show when={list().some((it) => !it.locked)}>
          <div style="display:flex;align-items:center;gap:10px;margin-top:13px">
            <span style="font-size:11px;color:var(--text-3);flex-grow:1">
              {dirtyOf(gp.group) ? '有改动还没保存，点右边生效' : '保存后立即生效，无需重启'}
            </span>
            <button
              class="hit btn btn-sm btn-primary"
              classList={{ loading: saving() === gp.group }}
              disabled={!dirtyOf(gp.group) || saving() !== ''}
              onClick={() => void saveGroup(gp.group)}
            >
              保存并生效
            </button>
          </div>
        </Show>
      </div>
    );
  };

  return (
    <Show when={ov()} fallback={<Placeholder err={err()} />}>
      <ProvidersCard
        onDirty={setProvDirty}
        onChanged={async () => {
          // 草稿不动，别处未保存的输入保留
          setItems(await adminGetConfig());
        }}
      />
      <TlsCard
        items={groupItems('server')}
        valOf={valOf}
        setVal={setVal}
        dirty={dirtyOf('server')}
        saving={saving() === 'server'}
        onSave={() => saveGroup('server')}
        onUploaded={() => syncGroup('server')}
      />
      <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(340px,1fr));gap:16px">
        <For each={groups()}>{(g) => <GroupCard group={g} />}</For>
        <div class="card">
          <div style="font-size:13px;font-weight:600;margin-bottom:5px">注册策略</div>
          <div style="font-size:11.5px;color:var(--text-2);margin-bottom:13px">决定新账号怎么来，改动立即生效</div>
          <div style="display:flex;flex-direction:column;gap:8px" role="radiogroup">
            <For each={POLICIES}>
              {(p) => {
                const on = () => policy() === p.id;
                return (
                  <button
                    class="hit"
                    role="radio"
                    aria-checked={on()}
                    disabled={policyBusy() !== ''}
                    style={`display:flex;align-items:flex-start;gap:11px;padding:11px 12px;border-radius:9px;border:1px solid ${on() ? 'var(--ember-line)' : 'var(--line)'};background:${on() ? 'var(--ember-tint)' : 'var(--bg-2)'};text-align:left;width:100%`}
                    onClick={() => {
                      if (!on()) void savePolicy(p.id, defaultRole(), p.id);
                    }}
                  >
                    <div class="radio" classList={{ on: on() }} style="margin-top:1px">
                      <div class="dot"></div>
                    </div>
                    <div style="flex-grow:1">
                      <div
                        style={`font-size:13px;font-weight:${on() ? 600 : 500};color:${on() ? 'var(--ember)' : 'var(--text-0)'}`}
                      >
                        {p.label}
                      </div>
                      <div style="font-size:11px;color:var(--text-2);margin-top:2px">{p.desc}</div>
                    </div>
                  </button>
                );
              }}
            </For>
          </div>
          <div style="display:flex;align-items:center;gap:10px;margin-top:13px">
            <span style="font-size:11.5px;color:var(--text-2);flex-shrink:0">注册默认档</span>
            <div class="seg-group" style="background:var(--bg-2);flex-grow:1">
              <For
                each={[
                  ['user', '普通用户'],
                  ['power', '高级用户'],
                ]}
              >
                {([v, label]) => (
                  <button
                    class="hit seg"
                    classList={{ on: defaultRole() === v }}
                    disabled={policyBusy() !== ''}
                    title={v === 'power' ? '新账号落地即可创建频道、发邀请' : '新账号不能创建频道，需要时由管理员提升'}
                    onClick={() => {
                      if (defaultRole() !== v) void savePolicy(policy(), v, `role-${v}`);
                    }}
                  >
                    {label}
                  </button>
                )}
              </For>
            </div>
          </div>
          <div style="margin-top:13px;font-size:11px;line-height:1.6;color:var(--text-3)">
            数据库：<span class="mono">{ov()!.services.db?.url ?? ''}</span>
            <br />
            媒体端口与域名在部署侧（.env / compose）改，改完重启容器生效。
          </div>
        </div>
      </div>
    </Show>
  );
}
