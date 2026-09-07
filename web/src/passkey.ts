// 通行密钥（Passkey / WebAuthn）的浏览器侧流程：begin → navigator.credentials → finish。
//
// 服务端直出的是 PublicKeyCredential*OptionsJSON（challenge / user.id / 凭证 id 都是
// base64url 字符串），浏览器要的是 ArrayBuffer。新浏览器有现成的转换器
// （PublicKeyCredential.parseCreationOptionsFromJSON / parseRequestOptionsFromJSON 与
// credential.toJSON()），有就用——它跟着规范走，比手写转换更不容易漏字段；
// 没有就退回下面的手工转换（只碰规范里明确是 base64url 的那几处）。
//
// 这些 API 比 TypeScript 自带的 DOM 声明新，所以这一层的类型断言比别处多；
// 断言只用于「探测并调用」，不做类型推导。
import type { PasskeyOptions, PasskeyRecord, User } from './api';
import {
  deletePasskey,
  listPasskeys,
  passkeyLoginBegin,
  passkeyLoginFinish,
  passkeyRegisterBegin,
  passkeyRegisterFinish,
  renamePasskey,
} from './api';

export type { PasskeyRecord };
export { deletePasskey, listPasskeys, renamePasskey };

// ---- base64url ----

function toBytes(s: string): Uint8Array {
  const pad = s.replace(/-/g, '+').replace(/_/g, '/');
  const bin = atob(pad + '='.repeat((4 - (pad.length % 4)) % 4));
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function toB64url(buf: ArrayBuffer | Uint8Array): string {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// ---- 能力探测 ----

interface PKCStatic {
  parseCreationOptionsFromJSON?: (o: unknown) => PublicKeyCredentialCreationOptions;
  parseRequestOptionsFromJSON?: (o: unknown) => PublicKeyCredentialRequestOptions;
  isConditionalMediationAvailable?: () => Promise<boolean>;
}

function pkc(): PKCStatic | undefined {
  return (window as unknown as { PublicKeyCredential?: PKCStatic }).PublicKeyCredential;
}

// isSupported 这台浏览器有没有 WebAuthn。
// 刻意不查 isUserVerifyingPlatformAuthenticatorAvailable()：那是「本机有没有指纹/面容」，
// 没有平台认证器时用手机扫码（浏览器自带的 hybrid 流程）照样能用，据此隐藏入口是误伤；
// 而且它是异步的，渲染路径要的是一个同步判断。
export function isSupported(): boolean {
  return typeof window !== 'undefined' && !!pkc() && !!navigator.credentials?.create;
}

// hasConditionalMediation 密码框的 autofill 里能不能顺带列出通行密钥。
export async function hasConditionalMediation(): Promise<boolean> {
  const fn = pkc()?.isConditionalMediationAvailable;
  if (!fn) return false;
  try {
    return await fn();
  } catch {
    return false;
  }
}

// ---- options / credential 的格式转换 ----

type IdItem = { id: string } & Record<string, unknown>;

function decodeIds(list: unknown): PublicKeyCredentialDescriptor[] | undefined {
  if (!Array.isArray(list)) return undefined;
  return (list as IdItem[]).map((c) => ({ ...c, id: toBytes(c.id) }) as unknown as PublicKeyCredentialDescriptor);
}

function creationOptions(o: PasskeyOptions): PublicKeyCredentialCreationOptions {
  const parse = pkc()?.parseCreationOptionsFromJSON;
  if (parse) return parse(o);
  const user = o.user as { id: string } & Record<string, unknown>;
  return {
    ...o,
    challenge: toBytes(o.challenge as string),
    user: { ...user, id: toBytes(user.id) },
    excludeCredentials: decodeIds(o.excludeCredentials),
  } as unknown as PublicKeyCredentialCreationOptions;
}

function requestOptions(o: PasskeyOptions): PublicKeyCredentialRequestOptions {
  const parse = pkc()?.parseRequestOptionsFromJSON;
  if (parse) return parse(o);
  return {
    ...o,
    challenge: toBytes(o.challenge as string),
    allowCredentials: decodeIds(o.allowCredentials),
  } as unknown as PublicKeyCredentialRequestOptions;
}

// credentialJSON 凭证 → 可 JSON 化的形状（服务端按 WebAuthn 的 JSON 编码解析）。
function credentialJSON(cred: Credential): unknown {
  const c = cred as PublicKeyCredential & { toJSON?: () => unknown };
  if (typeof c.toJSON === 'function') return c.toJSON();
  const base: Record<string, unknown> = {
    id: c.id,
    rawId: toB64url(c.rawId),
    type: c.type,
    clientExtensionResults: c.getClientExtensionResults(),
  };
  const attachment = (c as unknown as { authenticatorAttachment?: string | null }).authenticatorAttachment;
  if (attachment) base.authenticatorAttachment = attachment;
  const r = c.response;
  if ('attestationObject' in r) {
    const att = r as AuthenticatorAttestationResponse;
    const response: Record<string, unknown> = {
      clientDataJSON: toB64url(att.clientDataJSON),
      attestationObject: toB64url(att.attestationObject),
    };
    if (typeof att.getTransports === 'function') response.transports = att.getTransports();
    base.response = response;
    return base;
  }
  const asr = r as AuthenticatorAssertionResponse;
  base.response = {
    clientDataJSON: toB64url(asr.clientDataJSON),
    authenticatorData: toB64url(asr.authenticatorData),
    signature: toB64url(asr.signature),
    userHandle: asr.userHandle ? toB64url(asr.userHandle) : undefined,
  };
  return base;
}

// ---- 对外流程 ----

export interface LoginOpts {
  // conditional：让密码框的 autofill 里出现通行密钥（页面加载时静默发起，用户不点也不打扰）
  mediation?: CredentialMediationRequirement;
  signal?: AbortSignal;
}

// loginWithPasskey 一键登录：成功后本地会话已落地（与密码登录同一处理）。
export async function loginWithPasskey(opts: LoginOpts = {}): Promise<User> {
  const { ceremony_id, options } = await passkeyLoginBegin();
  const cred = await navigator.credentials.get({
    publicKey: requestOptions(options),
    mediation: opts.mediation,
    signal: opts.signal,
  });
  if (!cred) throw new Error('没有可用的通行密钥');
  return passkeyLoginFinish(ceremony_id, credentialJSON(cred));
}

// registerPasskey 给当前账号添加一枚。name 留空由服务端按 UA 起名。
export async function registerPasskey(name?: string): Promise<PasskeyRecord> {
  const { ceremony_id, options } = await passkeyRegisterBegin();
  const cred = await navigator.credentials.create({ publicKey: creationOptions(options) });
  if (!cred) throw new Error('浏览器没有创建通行密钥');
  return passkeyRegisterFinish(ceremony_id, credentialJSON(cred), name);
}

// passkeyErrorText 把浏览器抛的 DOMException 翻成一句人话；用户主动取消返回空串
// （取消不是错误，调用方据此静默收场）。
export function passkeyErrorText(err: unknown): string {
  const name = (err as { name?: string } | null)?.name;
  if (name === 'NotAllowedError' || name === 'AbortError') return '';
  return passkeyErrorDetail(err);
}

// passkeyErrorDetail 连取消/未完成也翻出来（名字 + 浏览器给的原文）：用户明确点了按钮就该有回音。
export function passkeyErrorDetail(err: unknown): string {
  const name = (err as { name?: string } | null)?.name;
  if (name === 'NotAllowedError') {
    const raw = (err as Error).message;
    return `没有完成通行密钥验证（NotAllowedError${raw ? '：' + raw : ''}）。若用的是密码管理器（如 Bitwarden），请确认它里面已有这个站点的通行密钥`;
  }
  if (name === 'AbortError') return '';
  if (name === 'InvalidStateError') return '这台设备上已经有这个账号的通行密钥了';
  if (name === 'SecurityError') return '当前地址不满足通行密钥的要求（需要 https，localhost 例外）';
  return (err as Error | null)?.message || '通行密钥操作失败';
}
