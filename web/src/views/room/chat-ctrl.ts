// 房间聊天控制：落库为权威、数据线实时扇出；文件字节只走数据通道。
// 信号与 DOM 引用由房间页持有，本模块通过参数读写，不另造真相源。
import type { Accessor, Setter } from 'solid-js';
import { createMemo } from 'solid-js';
import { deleteMessage, fetchMessages, postMessage, setReaction } from '../../chat';
import type { ChatMessage } from '../../chat';
import { compressImage } from '../../chat/compress';
import { applyMention, matchMentions, mentionQuery, mentionedUids, mentionsUser } from '../../chat/mentions';
import type { MentionQuery, MentionUser } from '../../chat/mentions';
import { encodeDelete, encodeMessage, encodeReaction, parseEnvelope } from '../../chat/protocol';
import { DATA_TOPIC_FILE, DATA_TOPIC_TEXT } from '../../engine/types';
import type { AVEngine } from '../../engine/types';
import { loadPrefs } from '../../prefs';
import { playCue } from '../../audio';
import { notifyMessage } from '../../notify';
import { confirmDialog, toast } from '../../ui';
import { mergeReaction } from './reactions';
import type { UnreadMarker } from './unread-divider';

// 与 room.tsx 同形的文件卡片字节状态（本模块不导出类型给 JSX，房间页自管渲染）
export type ChatFileStatus = 'sending' | 'ready' | 'receiving' | 'expired' | 'failed';
export interface ChatFileState {
  status: ChatFileStatus;
  progress: number;
  url: string;
}

// 客户端上限：与服务端 chat_file_max_mb 默认值一致
const FILE_MAX_MB = 25;

export interface ChatEls {
  log: HTMLDivElement;
  input: HTMLInputElement;
  file: HTMLInputElement;
}

export interface ChatCtrlOpts {
  channel: string;
  myUid: number;
  els: () => ChatEls;
  unreadMark: UnreadMarker;
  panel: Accessor<'members' | 'chat' | ''>;
  setPanel: Setter<'members' | 'chat' | ''>;
  muted: Accessor<boolean>;
  msgs: Accessor<ChatMessage[]>;
  setMsgs: Setter<ChatMessage[]>;
  setUnread: Setter<number>;
  setAtBottom: Setter<boolean>;
  setNewBelow: Setter<boolean>;
  setHistoryLoaded: Setter<boolean>;
  setChatReady: Setter<boolean>;
  setChatDrag: Setter<boolean>;
  replyToId: Accessor<number | null>;
  setReplyToId: Setter<number | null>;
  mentionQ: Accessor<MentionQuery | null>;
  setMentionQ: Setter<MentionQuery | null>;
  mentionIdx: Accessor<number>;
  setMentionIdx: Setter<number>;
  fileStates: Accessor<Map<number, ChatFileState>>;
  setFileStates: Setter<Map<number, ChatFileState>>;
  rosterMentionUsers: Accessor<MentionUser[]>;
  liveDataEngine: () => AVEngine | null;
  altEngine: (used: AVEngine | null) => AVEngine | null;
  isSelfGagged: () => boolean;
  leaving: () => boolean;
}

export interface ChatCtrl {
  switchPanel: (p: 'members' | 'chat' | '') => void;
  onChatScroll: () => void;
  jumpToLatest: () => void;
  appendMessage: (m: ChatMessage, live?: boolean) => void;
  onChatBroadcast: (text: string) => void;
  removeMessage: (m: ChatMessage) => Promise<void>;
  toggleReaction: (m: ChatMessage, emoji: string, on: boolean) => Promise<void>;
  jumpToMessage: (id: number) => void;
  syncMessages: (first: boolean) => Promise<void>;
  sendChat: (ev: Event) => Promise<void>;
  pickFiles: () => void;
  onChatPaste: (ev: ClipboardEvent) => void;
  onChatDrop: (ev: DragEvent) => void;
  syncMentionQuery: () => void;
  pickMention: (username: string) => void;
  onMentionKeyDown: (ev: KeyboardEvent) => boolean;
  mentionUsers: Accessor<MentionUser[]>;
  mentionHits: Accessor<MentionUser[]>;
  replyTarget: Accessor<ChatMessage | undefined>;
  putFileState: (id: number, st: ChatFileState) => void;
}

export function createChatCtrl(opts: ChatCtrlOpts): ChatCtrl {
  const NEAR_BOTTOM_PX = 48;

  function switchPanel(p: 'members' | 'chat' | '') {
    opts.setPanel(p);
    if (p === 'chat') {
      opts.setUnread(0);
      queueMicrotask(() => {
        const { log, input } = opts.els();
        log.scrollTop = log.scrollHeight;
        if (!window.matchMedia('(hover: none)').matches) input.focus();
      });
      opts.unreadMark.reveal();
    }
  }

  const onChatScroll = () => {
    const log = opts.els().log;
    const d = log.scrollHeight - log.scrollTop - log.clientHeight;
    const at = d <= NEAR_BOTTOM_PX;
    opts.setAtBottom(at);
    if (at) opts.setNewBelow(false);
    if (at) opts.unreadMark.atBottom();
  };

  function jumpToLatest() {
    opts.els().log.scrollTo({ top: opts.els().log.scrollHeight, behavior: 'smooth' });
  }

  function putFileState(id: number, st: ChatFileState) {
    opts.setFileStates((prev) => {
      const m = new Map(prev);
      const old = m.get(id);
      if (old?.url && old.url !== st.url) URL.revokeObjectURL(old.url);
      m.set(id, st);
      return m;
    });
  }

  function patchFileState(id: number, patch: Partial<ChatFileState>) {
    opts.setFileStates((prev) => {
      const cur = prev.get(id);
      if (!cur) return prev;
      const m = new Map(prev);
      m.set(id, { ...cur, ...patch });
      return m;
    });
  }

  function markExpired(list: ChatMessage[]) {
    for (const m of list) {
      if (m.kind === 'file' && !opts.fileStates().has(m.id)) {
        putFileState(m.id, { status: 'expired', progress: 0, url: '' });
      }
    }
  }

  const mentionUsers = createMemo<MentionUser[]>(() => opts.rosterMentionUsers());

  function replyToMe(m: ChatMessage): boolean {
    if (!m.reply_to) return false;
    return opts.msgs().find((x) => x.id === m.reply_to)?.uid === opts.myUid;
  }

  function onMention(_m: ChatMessage) {
    if (loadPrefs().mentionCue) playCue('mention');
  }

  const replyTarget = createMemo(() => {
    const id = opts.replyToId();
    return id ? opts.msgs().find((m) => m.id === id) : undefined;
  });

  const mentionHits = createMemo(() => {
    const q = opts.mentionQ();
    return q ? matchMentions(mentionUsers(), q.query) : [];
  });

  function syncMentionQuery() {
    const input = opts.els().input;
    const q = mentionQuery(input.value, input.selectionStart ?? input.value.length);
    opts.setMentionQ(q);
    opts.setMentionIdx(0);
  }

  function pickMention(username: string) {
    const q = opts.mentionQ();
    if (!q) return;
    const input = opts.els().input;
    const next = applyMention(input.value, q, username);
    input.value = next.value;
    input.setSelectionRange(next.caret, next.caret);
    opts.setChatReady(next.value.trim().length > 0);
    opts.setMentionQ(null);
    input.focus();
  }

  function onMentionKeyDown(ev: KeyboardEvent): boolean {
    const hits = mentionHits();
    if (!hits.length) return false;
    if (ev.key === 'Escape') {
      ev.preventDefault();
      opts.setMentionQ(null);
      return true;
    }
    if (ev.key === 'ArrowDown' || ev.key === 'ArrowUp') {
      ev.preventDefault();
      opts.setMentionIdx((i) => (i + (ev.key === 'ArrowDown' ? 1 : hits.length - 1)) % hits.length);
      return true;
    }
    if (ev.key === 'Enter' || ev.key === 'Tab') {
      ev.preventDefault();
      pickMention(hits[Math.min(opts.mentionIdx(), hits.length - 1)].username);
      return true;
    }
    return false;
  }

  let lastChatCueAt = 0;
  function playChatCue() {
    if (!loadPrefs().chatCue) return;
    const now = Date.now();
    if (now - lastChatCueAt < 1500) return;
    lastChatCueAt = now;
    playCue('message');
  }

  function appendMessage(m: ChatMessage, live = false) {
    let added = false;
    opts.setMsgs((list) => {
      if (list.some((x) => x.id === m.id)) return list;
      added = true;
      return [...list, m].sort((a, b) => a.id - b.id);
    });
    if (!added) return;
    if (m.kind === 'file' && !opts.fileStates().has(m.id)) {
      putFileState(m.id, { status: 'receiving', progress: 0, url: '' });
    }
    if (m.uid === opts.myUid) {
      queueMicrotask(() => {
        opts.els().log.scrollTop = opts.els().log.scrollHeight;
      });
      return;
    }
    const hidden = opts.panel() !== 'chat' || document.visibilityState !== 'visible';
    if (hidden) opts.setUnread((u) => u + 1);
    if (hidden) opts.unreadMark.note(m.id);
    if (opts.muted()) return;
    const mentioned = live && m.kind === 'text' && (mentionsUser(m.content, mentionUsers(), opts.myUid) || replyToMe(m));
    if (mentioned) onMention(m);
    else if (live && hidden) playChatCue();
    if (live) notifyMessage(m, mentioned, opts.channel, () => switchPanel('chat'));
  }

  function patchMessage(id: number, patch: Partial<ChatMessage> | ((m: ChatMessage) => Partial<ChatMessage>)) {
    opts.setMsgs((list) => {
      const i = list.findIndex((x) => x.id === id);
      if (i < 0) return list;
      const next = [...list];
      next[i] = { ...list[i], ...(typeof patch === 'function' ? patch(list[i]) : patch) };
      return next;
    });
  }

  function broadcast(text: string) {
    const eng = opts.liveDataEngine();
    if (!eng) return;
    void eng.sendText(DATA_TOPIC_TEXT, text).catch(() => {
      if (opts.isSelfGagged()) return;
      const alt = opts.altEngine(eng);
      if (!alt) return;
      void alt.sendText(DATA_TOPIC_TEXT, text).catch(() => {});
    });
  }

  function onChatBroadcast(text: string) {
    const env = parseEnvelope(text);
    if (!env) return;
    if (env.t === 'message') {
      appendMessage(env.m, true);
      return;
    }
    if (env.t === 'delete') {
      patchMessage(env.id, { deleted: true, content: '', file: undefined, reactions: [] });
      return;
    }
    patchMessage(env.id, (m) => ({ reactions: mergeReaction(m.reactions, env.emoji, env.uid, env.on) }));
  }

  async function removeMessage(m: ChatMessage) {
    const ok = await confirmDialog({
      title: m.uid === opts.myUid ? '撤回这条消息？' : `删除 ${m.username} 的这条消息？`,
      body: '消息内容会从服务器上清掉，聊天里只留一条"已撤回"的占位。',
      danger: true,
      confirmText: m.uid === opts.myUid ? '撤回' : '删除',
    });
    if (!ok) return;
    try {
      await deleteMessage(opts.channel, m.id);
    } catch (err) {
      toast((err as Error).message, 'bad');
      return;
    }
    patchMessage(m.id, { deleted: true, content: '', file: undefined, reactions: [] });
    if (opts.replyToId() === m.id) opts.setReplyToId(null);
    broadcast(encodeDelete(m.id, opts.myUid));
  }

  async function toggleReaction(m: ChatMessage, emoji: string, on: boolean) {
    patchMessage(m.id, (cur) => ({ reactions: mergeReaction(cur.reactions, emoji, opts.myUid, on) }));
    try {
      const res = await setReaction(opts.channel, m.id, emoji, on);
      patchMessage(m.id, { reactions: res.reactions });
    } catch (err) {
      patchMessage(m.id, (cur) => ({ reactions: mergeReaction(cur.reactions, emoji, opts.myUid, !on) }));
      toast((err as Error).message, 'bad');
      return;
    }
    broadcast(encodeReaction(m.id, emoji, opts.myUid, on));
  }

  function jumpToMessage(id: number) {
    const node = opts.els().log.querySelector<HTMLElement>(`[data-msg-id="${id}"]`);
    if (!node) return;
    node.scrollIntoView({ block: 'center', behavior: 'smooth' });
    node.classList.add('flash');
    setTimeout(() => node.classList.remove('flash'), 1200);
  }

  let syncing = false;
  async function syncMessages(first: boolean) {
    if (syncing || opts.leaving()) return;
    syncing = true;
    try {
      const after = first ? 0 : opts.msgs().reduce((max, m) => (m.id > max ? m.id : max), 0);
      const list = await fetchMessages(opts.channel, after);
      if (opts.leaving()) return;
      markExpired(list);
      if (first) {
        opts.setMsgs(list);
        opts.setHistoryLoaded(true);
        opts.setUnread(0);
      } else {
        list.forEach((m) => appendMessage(m));
      }
    } catch {
      // 取历史失败不阻断进房
    } finally {
      syncing = false;
    }
  }

  let sendingText = false;
  async function sendChat(ev: Event) {
    ev.preventDefault();
    const input = opts.els().input;
    const content = input.value.trim();
    if (!content || sendingText) return;
    sendingText = true;
    const quoted = opts.replyToId();
    input.value = '';
    opts.setChatReady(false);
    opts.setMentionQ(null);
    opts.setReplyToId(null);
    try {
      const mentions = mentionedUids(content, mentionUsers());
      const m = await postMessage(opts.channel, quoted ? { content, reply_to: quoted, mentions } : { content, mentions });
      appendMessage(m);
      broadcast(encodeMessage(m));
    } catch (err) {
      input.value = content;
      opts.setChatReady(true);
      if (quoted) opts.setReplyToId(quoted);
      toast((err as Error).message, 'bad');
    } finally {
      sendingText = false;
    }
  }

  async function sendFiles(raws: File[]) {
    for (const raw of raws) {
      const f = await compressImage(raw);
      if (f.size > FILE_MAX_MB * 1024 * 1024) {
        toast(`「${f.name}」超过 ${FILE_MAX_MB} MB，发不出去`, 'bad');
        continue;
      }
      await sendOneFile(f);
    }
  }

  async function sendOneFile(file: File) {
    const meta = { name: file.name, mime: file.type || 'application/octet-stream', size: file.size };
    let m: ChatMessage;
    try {
      m = await postMessage(opts.channel, { kind: 'file', file: meta });
    } catch (err) {
      toast((err as Error).message, 'bad');
      return;
    }
    putFileState(m.id, { status: 'sending', progress: 0, url: URL.createObjectURL(file) });
    appendMessage(m);
    broadcast(encodeMessage(m));
    const eng = opts.liveDataEngine();
    if (!eng) {
      patchFileState(m.id, { status: 'ready', progress: 1 });
      toast('数据线未连接，对方只会看到一张过期卡片', 'bad');
      return;
    }
    const sendOn = (on: AVEngine) =>
      on.sendFile(file, DATA_TOPIC_FILE, { message_id: String(m.id) }, (p) => patchFileState(m.id, { progress: p }));
    try {
      await sendOn(eng);
      patchFileState(m.id, { status: 'ready', progress: 1 });
      return;
    } catch (err) {
      const alt = opts.isSelfGagged() ? null : opts.altEngine(eng);
      if (alt) {
        try {
          patchFileState(m.id, { progress: 0 });
          await sendOn(alt);
          patchFileState(m.id, { status: 'ready', progress: 1 });
          return;
        } catch {
          // 落到下面统一标失败
        }
      }
      patchFileState(m.id, { status: 'failed' });
      toast(`「${file.name}」发送失败：${(err as Error)?.message ?? ''}`, 'bad');
    }
  }

  const pickFiles = () => {
    const fileEl = opts.els().file;
    const list = [...(fileEl.files ?? [])];
    fileEl.value = '';
    if (list.length) void sendFiles(list);
  };

  const onChatPaste = (ev: ClipboardEvent) => {
    const items = ev.clipboardData?.items;
    const list = items
      ? [...items].filter((it) => it.kind === 'file').map((it) => it.getAsFile()).filter((f): f is File => !!f)
      : [...(ev.clipboardData?.files ?? [])];
    if (!list.length) return;
    ev.preventDefault();
    void sendFiles(list);
  };

  const onChatDrop = (ev: DragEvent) => {
    ev.preventDefault();
    opts.setChatDrag(false);
    const list = [...(ev.dataTransfer?.files ?? [])];
    if (list.length) void sendFiles(list);
  };

  return {
    switchPanel,
    onChatScroll,
    jumpToLatest,
    appendMessage,
    onChatBroadcast,
    removeMessage,
    toggleReaction,
    jumpToMessage,
    syncMessages,
    sendChat,
    pickFiles,
    onChatPaste,
    onChatDrop,
    syncMentionQuery,
    pickMention,
    onMentionKeyDown,
    mentionUsers,
    mentionHits,
    replyTarget,
    putFileState,
  };
}
