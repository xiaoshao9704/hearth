// 管理后台「服务实例」卡片：内核实例列表与 DB 注册 CRUD。
import { createEffect, createMemo, createSignal, For, Show } from 'solid-js';
import {
  adminCreateProvider,
  adminDeleteProvider,
  adminListProviders,
  adminUpdateProvider,
} from '../../api';
import type { ProviderField, ProviderInstance, ProviderType } from '../../api';
import { confirmDialog, toast } from '../../ui';

const CAP_LABELS: Record<string, string> = { voice: '语音', stage: '舞台', ingest: '推流' };
// 内建类型不在可注册类型列表里，展示名这里补
const TYPE_LABELS: Record<string, string> = {
  'livekit-embedded': 'LiveKit（进程内）',
};

export function ProvidersCard(props: {
  onDirty: (dirty: boolean) => void;
  // 实例增删改后选择器可选项跟着变，让宿主把配置项重拉（草稿不动）
  onChanged: () => Promise<void>;
}) {
  const [instances, setInstances] = createSignal<ProviderInstance[]>([]);
  const [types, setTypes] = createSignal<ProviderType[]>([]);
  const [aliasPattern, setAliasPattern] = createSignal(''); // 以接口 alias_pattern 为准，未下发则不可提交
  const [pMode, setPMode] = createSignal<'create' | 'edit'>('create');
  const [pAlias, setPAlias] = createSignal('');
  const [pType, setPType] = createSignal('');
  const [pValues, setPValues] = createSignal<Record<string, string>>({});
  const [provBusy, setProvBusy] = createSignal(false);
  const [delBusy, setDelBusy] = createSignal(''); // 正在删除的实例 alias
  let provCard!: HTMLDivElement;

  const reload = async () => {
    const provs = await adminListProviders();
    setInstances(provs.instances);
    setTypes(provs.types);
    setAliasPattern(provs.alias_pattern);
  };

  void (async () => {
    try {
      await reload();
      setPType(types()[0]?.type ?? '');
    } catch (e) {
      toast((e as Error).message, 'bad');
    }
  })();

  const typeLabel = (t: string) => types().find((x) => x.type === t)?.label ?? TYPE_LABELS[t] ?? t;
  const fieldsOf = (t: string) => types().find((x) => x.type === t)?.fields ?? [];

  const resetProviderForm = () => {
    setPMode('create');
    setPAlias('');
    setPType(types()[0]?.type ?? '');
    setPValues({});
  };

  const editing = () => (pMode() === 'edit' ? instances().find((i) => i.alias === pAlias()) : undefined);

  // 实例表单是否有未提交的改动：创建态看是否已填过什么，编辑态跟已保存的 params 逐字段比
  const providerDirty = createMemo(() => {
    const inst = editing();
    if (inst) {
      return fieldsOf(inst.type).some((f) => {
        const v = pValues()[f.name] ?? '';
        return f.secret ? v.trim() !== '' : v !== (inst.params[f.name] ?? '');
      });
    }
    return pAlias().trim() !== '' || Object.values(pValues()).some((v) => v.trim() !== '');
  });
  createEffect(() => props.onDirty(providerDirty()));

  // 必填 / 格式一律按接口下发的字段模式（optional / pattern），不在页面复刻类型特例
  const fieldMissing = (f: ProviderField) => {
    const v = (pValues()[f.name] ?? '').trim();
    if (f.secret) return pMode() === 'create' && v === '';
    if (f.optional) return false;
    return v === '';
  };
  const fieldBadPattern = (f: ProviderField) => {
    const v = (pValues()[f.name] ?? '').trim();
    if (!v || !f.pattern) return false;
    try {
      return !new RegExp(f.pattern).test(v);
    } catch {
      return true;
    }
  };
  const aliasValid = () => {
    const v = pAlias().trim();
    const pat = aliasPattern();
    if (!v || !pat) return false;
    try {
      return new RegExp(pat).test(v);
    } catch {
      return false;
    }
  };
  const canSubmit = createMemo(() => {
    if (pMode() === 'create' && !aliasValid()) return false;
    return fieldsOf(pType()).every((f) => !fieldMissing(f) && !fieldBadPattern(f));
  });

  const afterMutation = async () => {
    await reload();
    await props.onChanged();
  };

  const submitProvider = async () => {
    if (!canSubmit() || provBusy()) return;
    // 全量替换语义：该类型全部字段都提交
    const params: Record<string, string> = {};
    for (const f of fieldsOf(pType())) params[f.name] = (pValues()[f.name] ?? '').trim();
    setProvBusy(true);
    try {
      if (pMode() === 'edit') {
        await adminUpdateProvider(pAlias(), params);
        toast('实例已保存并生效', 'ok');
      } else {
        await adminCreateProvider({ type: pType(), alias: pAlias().trim(), params });
        toast('实例已注册', 'ok');
      }
      resetProviderForm();
      await afterMutation();
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setProvBusy(false);
    }
  };

  const deleteProvider = async (alias: string) => {
    if (delBusy()) return;
    const ok = await confirmDialog({
      title: `删除实例 ${alias}？`,
      body: '选择器仍引用它时服务端会拒绝；删除不可恢复。',
      danger: true,
      confirmText: '删除',
    });
    if (!ok) return;
    setDelBusy(alias);
    try {
      await adminDeleteProvider(alias);
      if (pMode() === 'edit' && pAlias() === alias) resetProviderForm();
      await afterMutation();
      toast('实例已删除', 'ok');
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setDelBusy('');
    }
  };

  // 切换实例类型时已填参数作废：已经填了点什么就先确认一次
  const switchType = async (t: string) => {
    if (pType() === t) return;
    const hasValues = Object.values(pValues()).some((v) => v.trim() !== '');
    if (hasValues) {
      const ok = await confirmDialog({
        title: '切换实例类型？',
        body: '已填写的参数会被清空（alias 保留）。',
        danger: true,
        confirmText: '切换并清空',
      });
      if (!ok) return;
    }
    setPType(t);
    setPValues({});
  };

  const startEdit = (inst: ProviderInstance) => {
    // params 里 Secret 已被服务端掩码为空串，提交时空串 = 保留旧值
    setPMode('edit');
    setPAlias(inst.alias);
    setPType(inst.type);
    setPValues({ ...inst.params });
    provCard?.scrollIntoView({ block: 'nearest' });
  };

  const fieldInput = (f: ProviderField) => {
    const placeholder = () =>
      f.secret && pMode() === 'edit' && editing()?.params_set[f.name] ? '已设置（留空保持不变）' : f.hint;
    const bad = () => fieldMissing(f) || fieldBadPattern(f);
    return (
      <div>
        <div style="display:flex;align-items:baseline;gap:8px;margin-bottom:6px">
          <span style="font-size:11px;color:var(--text-2)">{f.label}</span>
          <span class="mono" style="font-size:10px;color:var(--text-3)">{f.name}</span>
        </div>
        <div class="field" classList={{ bad: bad() }} style="height:38px;background:var(--bg-2)">
          <input
            class="mono"
            style="font-size:12px"
            type={f.secret ? 'password' : 'text'}
            value={pValues()[f.name] ?? ''}
            placeholder={placeholder()}
            autocomplete="off"
            onInput={(ev) => setPValues((v) => ({ ...v, [f.name]: ev.currentTarget.value }))}
          />
        </div>
      </div>
    );
  };

  return (
    <div class="card" id="prov-card" ref={provCard} style="padding:18px 20px">
      <div style="display:flex;align-items:baseline;gap:9px;margin-bottom:4px">
        <div style="font-size:13px;font-weight:600">服务实例</div>
        <div style="font-size:11px;color:var(--text-2)">语音 / 舞台 / 推流接入的内核实例；内建与环境变量锁定的只读</div>
      </div>
      <div
        class="table-box"
        style={{ 'margin-top': '11px', '--col-1': '170px', '--col-2': '170px', '--col-4': '90px', '--col-5': '150px' }}
      >
        <div class="table-head">
          <div>alias</div>
          <div>类型</div>
          <div style="flex-grow:1">能力</div>
          <div>来源</div>
          <div style="text-align:right">操作</div>
        </div>
        <Show when={instances().length > 0} fallback={<div class="table-empty">没有实例。</div>}>
          <For each={instances()}>
            {(inst) => (
              <div class="table-row">
                <div class="mono cell-ellipsis" data-label="alias" style="font-size:12.5px;font-weight:600;color:var(--text-0)">
                  {inst.alias}
                </div>
                <div data-label="类型" style="font-size:12px;color:var(--text-1)">{typeLabel(inst.type)}</div>
                <div data-label="能力" style="flex-grow:1;font-size:12px;color:var(--text-1)">
                  {inst.caps.map((c) => CAP_LABELS[c] ?? c).join(' / ') || '—'}
                </div>
                <div data-label="来源">
                  {inst.builtin ? (
                    <span class="chip tag-ember">内建</span>
                  ) : inst.locked ? (
                    <span
                      class="chip"
                      style="background:var(--bg-4);color:var(--text-1)"
                      title="部署侧 .env / compose 里改，重启生效"
                    >
                      环境锁定
                    </span>
                  ) : (
                    <span class="chip tag-sage">DB</span>
                  )}
                </div>
                <div class="table-actions">
                  <Show when={!inst.builtin && !inst.locked}>
                    <button class="hit btn btn-sm" disabled={delBusy() !== ''} onClick={() => startEdit(inst)}>
                      编辑
                    </button>
                    <button
                      class="hit btn btn-sm btn-danger"
                      classList={{ loading: delBusy() === inst.alias }}
                      disabled={delBusy() !== ''}
                      onClick={() => void deleteProvider(inst.alias)}
                    >
                      删除
                    </button>
                  </Show>
                </div>
              </div>
            )}
          </For>
        </Show>
      </div>
      <Show
        when={pMode() === 'edit' && editing()}
        fallback={
          <form
            style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px"
            onSubmit={(ev) => {
              ev.preventDefault();
              void submitProvider();
            }}
          >
            <div style="display:flex;align-items:baseline;gap:9px">
              <div style="font-size:12.5px;font-weight:600">注册新实例</div>
              <div style="font-size:11px;color:var(--text-3)">注册后在「内核选择」里按 alias 选用</div>
            </div>
            <div style="display:flex;gap:20px;margin-top:12px;align-items:flex-end;flex-wrap:wrap">
              <div>
                <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">类型</div>
                <div class="seg-group" style="background:var(--bg-2)">
                  <For each={types()}>
                    {(t) => (
                      <button type="button" class="hit seg" classList={{ on: pType() === t.type }} onClick={() => void switchType(t.type)}>
                        {t.label}
                      </button>
                    )}
                  </For>
                </div>
              </div>
              <div style="flex-grow:1;min-width:220px">
                <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">
                  alias（小写字母数字与 -，会出现在 /providers/&lt;alias&gt; 连接路径里）
                </div>
                <div class="field" classList={{ bad: pAlias().trim() !== '' && !aliasValid() }} style="height:38px;background:var(--bg-2)">
                  <input
                    class="mono"
                    style="font-size:12px"
                    id="prov-alias"
                    value={pAlias()}
                    placeholder="如 lk-main"
                    onInput={(ev) => setPAlias(ev.currentTarget.value)}
                  />
                </div>
              </div>
            </div>
            <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:11px;margin-top:12px">
              <For each={fieldsOf(pType())}>{(f) => fieldInput(f)}</For>
            </div>
            <div style="display:flex;justify-content:flex-end;margin-top:13px">
              <button type="submit" class="hit btn btn-sm btn-primary" classList={{ loading: provBusy() }} disabled={!canSubmit() || provBusy()}>
                注册实例
              </button>
            </div>
          </form>
        }
      >
        <div style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px">
          <div style="display:flex;align-items:baseline;gap:9px">
            <div style="font-size:12.5px;font-weight:600">
              编辑实例 <span class="mono">{editing()!.alias}</span>（{typeLabel(editing()!.type)}）
            </div>
            <div style="font-size:11px;color:var(--text-3)">保存为全量替换；密钥留空 = 保持不变</div>
            <div class="spacer"></div>
            <button class="hit btn btn-sm" onClick={resetProviderForm}>
              取消
            </button>
          </div>
          <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:11px;margin-top:12px">
            <For each={fieldsOf(editing()!.type)}>{(f) => fieldInput(f)}</For>
          </div>
          <div style="display:flex;justify-content:flex-end;margin-top:13px">
            <button
              class="hit btn btn-sm btn-primary"
              classList={{ loading: provBusy() }}
              disabled={!canSubmit() || provBusy()}
              onClick={() => void submitProvider()}
            >
              保存并生效
            </button>
          </div>
        </div>
      </Show>
    </div>
  );
}
