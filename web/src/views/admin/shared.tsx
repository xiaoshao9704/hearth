// 管理后台各 tab 共用的加载/出错占位。
import { Show } from 'solid-js';

export function Placeholder(props: { err: string }) {
  return (
    <Show when={props.err} fallback={<div class="muted">加载中…</div>}>
      <div class="error-text">{props.err}</div>
    </Show>
  );
}
