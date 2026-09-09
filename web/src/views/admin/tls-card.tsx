// 管理后台「服务器」分区：TLS 证书来源、根证书管理、对外地址与端口映射诊断。
// 来源三个 dyncfg 键（tls_cert_source/tls_cert_file/tls_key_file）复用 ConfigTab 已有的
// 草稿/保存机制（valOf/setVal/onSave），本组件自己只管 GET /api/admin/tls 状态与上传/轮换。
import { createSignal, For, Show } from 'solid-js';
import { getTls, rotateTlsCA, SERVER_URL, uploadTls } from '../../api';
import type { ConfigItem, TlsStatus } from '../../api';
import { confirmDialog, copyText, el, icon, toast } from '../../ui';

const SOURCE_OPTS = ['off', 'self', 'file', 'upload'] as const;
const SOURCE_LABELS: Record<string, string> = { off: '关闭', self: '自签', file: '证书文件', upload: '手动上传' };

// 与 server/internal/portmap.Diagnosis 对应的人话标签
const DIAG_LABELS: Record<string, string> = {
  ok: '正常',
  off: '未启用',
  no_gateway: '找不到网关',
  disabled_by_gateway: '网关已禁用转发',
  upstream_nat: '上游还有一层 NAT',
  port_conflict: '外部端口冲突',
  host_firewall: '本机防火墙',
  error: '出错',
};
// 沿用既有 chip 三色：ok 用 sage，off/未知中性，其余问题态用 red
const diagClass = (d: string) => (d === 'ok' ? 'tag-sage' : d === 'off' ? '' : 'tag-red');

function fmtDate(iso: string): string {
  if (!iso) return '—';
  const t = new Date(iso);
  return Number.isNaN(t.getTime()) ? iso : t.toLocaleString();
}

export function TlsCard(props: {
  items: ConfigItem[]; // group=server 的三个键
  valOf: (it: ConfigItem) => string;
  setVal: (it: ConfigItem, v: string) => void;
  dirty: boolean;
  saving: boolean;
  onSave: () => Promise<void>;
}) {
  const [status, setStatus] = createSignal<TlsStatus>();
  const [err, setErr] = createSignal('');
  const [uploadCert, setUploadCert] = createSignal<File | null>(null);
  const [uploadKey, setUploadKey] = createSignal<File | null>(null);
  const [uploadBusy, setUploadBusy] = createSignal(false);
  const [rotateBusy, setRotateBusy] = createSignal(false);
  let certFileEl!: HTMLInputElement;
  let keyFileEl!: HTMLInputElement;

  const refresh = async () => {
    try {
      setStatus(await getTls());
      setErr('');
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  void refresh();

  const sourceItem = () => props.items.find((it) => it.name === 'tls_cert_source');
  const certFileItem = () => props.items.find((it) => it.name === 'tls_cert_file');
  const keyFileItem = () => props.items.find((it) => it.name === 'tls_key_file');
  // 草稿优先：切来源立刻切换下方表单，不等保存；items 还没到位时按状态接口的当前来源兜底
  const sourceVal = () => {
    const it = sourceItem();
    return it ? props.valOf(it) : (status()?.source ?? 'self');
  };

  const doSave = async () => {
    await props.onSave();
    await refresh();
  };

  const rotate = async () => {
    if (rotateBusy()) return;
    const ok = await confirmDialog({
      title: '重新生成根证书？',
      body: '旧根证书签发的证书立即失效，已经装过这份根证书的朋友设备需要重新下载安装，否则会看到不安全提示。仅在私钥可能泄漏或约束需要更新时使用。',
      danger: true,
      confirmText: '重新生成',
    });
    if (!ok) return;
    setRotateBusy(true);
    try {
      setStatus(await rotateTlsCA());
      toast('根证书已重新生成，记得提醒已装过的设备重装', 'ok');
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setRotateBusy(false);
    }
  };

  const submitUpload = async () => {
    const cert = uploadCert();
    const key = uploadKey();
    if (!cert || !key || uploadBusy()) return;
    setUploadBusy(true);
    try {
      setStatus(await uploadTls(cert, key));
      setUploadCert(null);
      setUploadKey(null);
      toast('证书已上传并生效', 'ok');
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setUploadBusy(false);
    }
  };

  const copyFp = async (fp: string) => {
    if (await copyText(fp)) toast('已复制指纹', 'ok', 1400);
  };

  return (
    <div class="card" style="padding:18px 20px">
      <div style="display:flex;align-items:baseline;gap:9px;margin-bottom:4px">
        <div style="font-size:13px;font-weight:600">TLS 与对外地址</div>
        <div style="font-size:11px;color:var(--text-2)">证书来源、根证书与端口映射状态</div>
        <Show when={props.dirty}>
          <span class="tag tag-ember">未保存</span>
        </Show>
      </div>

      <div style="margin-top:11px">
        <div style="display:flex;align-items:baseline;gap:8px;margin-bottom:7px">
          <span style="font-size:11px;color:var(--text-2)">证书来源</span>
          <Show when={sourceItem()?.locked}>
            <span class="tag" title="部署侧 .env / compose 里改，重启生效">环境变量固定</span>
          </Show>
        </div>
        <Show
          when={!sourceItem()?.locked}
          fallback={
            <div
              class="mono"
              style="padding:9px 12px;border-radius:8px;background:var(--bg-2);border:1px solid var(--line);font-size:12px;color:var(--text-1)"
            >
              {SOURCE_LABELS[sourceVal()] ?? sourceVal()}
            </div>
          }
        >
          <div class="seg-group" style="background:var(--bg-2)">
            <For each={SOURCE_OPTS}>
              {(opt) => (
                <button
                  type="button"
                  class="hit seg"
                  classList={{ on: sourceVal() === opt }}
                  onClick={() => sourceItem() && props.setVal(sourceItem()!, opt)}
                >
                  {SOURCE_LABELS[opt]}
                </button>
              )}
            </For>
          </div>
        </Show>
      </div>

      <Show when={sourceVal() === 'file' && !sourceItem()?.locked}>
        <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:11px;margin-top:12px">
          <For each={[certFileItem(), keyFileItem()]}>
            {(it) =>
              it && (
                <div>
                  <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">{it.label}</div>
                  <div class="field" style="height:38px;background:var(--bg-2)">
                    <input
                      class="mono"
                      style="font-size:12px"
                      value={props.valOf(it)}
                      placeholder={it.hint}
                      autocomplete="off"
                      onInput={(ev) => props.setVal(it, ev.currentTarget.value)}
                    />
                  </div>
                </div>
              )
            }
          </For>
        </div>
      </Show>

      <Show when={!sourceItem()?.locked && props.items.length > 0}>
        <div style="display:flex;align-items:center;gap:10px;margin-top:13px">
          <span style="font-size:11px;color:var(--text-3);flex-grow:1">
            {props.dirty ? '有改动还没保存，点右边生效' : '保存后立即生效，无需重启'}
          </span>
          <button
            class="hit btn btn-sm btn-primary"
            classList={{ loading: props.saving }}
            disabled={!props.dirty || props.saving}
            onClick={() => void doSave()}
          >
            保存并生效
          </button>
        </div>
      </Show>

      <Show when={sourceVal() === 'upload'}>
        <div style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px">
          <div style="font-size:12.5px;font-weight:600;margin-bottom:9px">上传证书</div>
          <div style="display:flex;gap:11px;flex-wrap:wrap;align-items:center">
            <button type="button" class="hit btn btn-sm" onClick={() => certFileEl.click()}>
              选证书文件
            </button>
            <span style="font-size:11.5px;color:var(--text-2)">{uploadCert()?.name ?? '未选择'}</span>
            <button type="button" class="hit btn btn-sm" onClick={() => keyFileEl.click()}>
              选私钥文件
            </button>
            <span style="font-size:11.5px;color:var(--text-2)">{uploadKey()?.name ?? '未选择'}</span>
            <button
              type="button"
              class="hit btn btn-sm btn-primary"
              classList={{ loading: uploadBusy() }}
              disabled={!uploadCert() || !uploadKey() || uploadBusy()}
              onClick={() => void submitUpload()}
            >
              上传并生效
            </button>
          </div>
          <input ref={certFileEl} type="file" accept=".crt,.pem,.cer" hidden onChange={(ev) => setUploadCert(ev.currentTarget.files?.[0] ?? null)} />
          <input ref={keyFileEl} type="file" accept=".key,.pem" hidden onChange={(ev) => setUploadKey(ev.currentTarget.files?.[0] ?? null)} />
        </div>
      </Show>

      <Show when={status()} fallback={<Show when={err()}><div class="error-text" style="margin-top:13px">{err()}</div></Show>}>
        {(st) => (
          <>
            <Show when={st().source === 'off'}>
              <div class="hint-card" style="margin-top:16px">TLS 已关闭，当前仅明文访问。</div>
            </Show>

            <Show when={st().source !== 'off'}>
              <div style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px">
                <div style="font-size:12.5px;font-weight:600;margin-bottom:9px">证书</div>
                <Show
                  when={st().cert}
                  fallback={
                    <div class="notice-bad">
                      {el(icon('warn', 15))}
                      <span>当前没有可用证书。</span>
                    </div>
                  }
                >
                  {(cert) => (
                    <div style="display:flex;flex-direction:column;gap:6px;font-size:12px;color:var(--text-1)">
                      <div>
                        主体：<span class="mono">{cert().subject}</span>
                      </div>
                      <div>
                        SAN：<span class="mono">{cert().sans.join(', ') || '—'}</span>
                      </div>
                      <div>
                        到期：<span class="mono">{fmtDate(cert().not_after)}</span>
                      </div>
                      <div style="display:flex;align-items:center;gap:8px">
                        指纹：<span class="mono">{cert().fingerprint_sha256}</span>
                        <button type="button" class="hit btn btn-sm" onClick={() => void copyFp(cert().fingerprint_sha256)}>
                          {el(icon('copy', 12))} 复制
                        </button>
                      </div>
                    </div>
                  )}
                </Show>

                <Show when={st().ca}>
                  {(ca) => (
                    <div style="margin-top:13px;padding-top:13px;border-top:1px solid var(--line-soft)">
                      <div style="font-size:12.5px;font-weight:600;margin-bottom:9px">根证书</div>
                      <Show when={ca().constraint_stale}>
                        <div class="notice-bad" style="margin-bottom:10px">
                          {el(icon('warn', 15))}
                          <span>新增主机名需要重新生成根证书，朋友设备要重装。</span>
                        </div>
                      </Show>
                      <div style="display:flex;flex-direction:column;gap:6px;font-size:12px;color:var(--text-1)">
                        <div>
                          有效期至：<span class="mono">{fmtDate(ca().not_after)}</span>
                        </div>
                        <div style="display:flex;align-items:center;gap:8px">
                          指纹：<span class="mono">{ca().fingerprint_sha256}</span>
                          <button type="button" class="hit btn btn-sm" onClick={() => void copyFp(ca().fingerprint_sha256)}>
                            {el(icon('copy', 12))} 复制
                          </button>
                        </div>
                      </div>
                      <div style="display:flex;gap:9px;margin-top:11px;flex-wrap:wrap">
                        <a class="hit btn btn-sm" href={`${SERVER_URL}/ca.crt`}>
                          {el(icon('install', 13))} 下载根证书
                        </a>
                        <a class="hit btn btn-sm" href={`${SERVER_URL}/ca`} target="_blank" rel="noopener">
                          安装说明页
                        </a>
                        <button
                          type="button"
                          class="hit btn btn-sm btn-danger"
                          classList={{ loading: rotateBusy() }}
                          disabled={rotateBusy()}
                          onClick={() => void rotate()}
                        >
                          重新生成根证书
                        </button>
                      </div>
                    </div>
                  )}
                </Show>
              </div>
            </Show>

            <div style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px">
              <div style="font-size:12.5px;font-weight:600;margin-bottom:9px">对外地址</div>
              <Show
                when={st().external.addresses.length > 0}
                fallback={<div style="font-size:12px;color:var(--text-2)">还没探测到公网地址。</div>}
              >
                <div style="display:flex;flex-wrap:wrap;gap:7px">
                  <For each={st().external.addresses}>{(a) => <span class="chip mono">{a}</span>}</For>
                </div>
              </Show>
              <div style="font-size:10.5px;color:var(--text-3);margin-top:6px">
                {st().external.probed_at ? `探测于 ${fmtDate(st().external.probed_at)}` : ''}
              </div>
            </div>

            <div style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px">
              <div style="display:flex;align-items:baseline;gap:8px;margin-bottom:9px">
                <div style="font-size:12.5px;font-weight:600">端口映射</div>
                <span class={`chip ${diagClass(st().portmap.diagnosis)}`}>{DIAG_LABELS[st().portmap.diagnosis] ?? st().portmap.diagnosis}</span>
              </div>
              <div style="font-size:12px;color:var(--text-2);line-height:1.6">{st().portmap.detail || '—'}</div>
              <Show when={st().portmap.v6_detail}>
                <div style="font-size:11.5px;color:var(--text-3);margin-top:4px">IPv6：{st().portmap.v6_detail}</div>
              </Show>
              <Show when={st().portmap.pinholes.length > 0}>
                <div style="display:flex;flex-wrap:wrap;gap:6px;margin-top:8px">
                  <For each={st().portmap.pinholes}>
                    {(p) => (
                      <span class="chip mono">
                        {p.proto}/{p.port}
                      </span>
                    )}
                  </For>
                </div>
              </Show>
            </div>
          </>
        )}
      </Show>
    </div>
  );
}
