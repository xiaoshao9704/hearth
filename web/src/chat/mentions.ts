// @提及：解析、高亮分段与输入框补全的纯字符串逻辑（不碰 DOM，房间页只负责渲染）。
//
// 一条约束贯穿全文件：**匹配的是名册里真实存在的用户名，不是某个通配的用户名正则**。
// 用户名字符集含 `-`、可以随时改名，正则切出来的"疑似用户名"既可能切错边界，
// 也没法映射回 uid；而被@判定必须按 uid（见 CLAUDE.md 的身份键约束）。
// 代价是名册里没有的人（离线、没进房）@不出高亮——这正确：提示不到的人不该显示成提示到了。

export interface MentionUser {
  uid: number;
  username: string;
}

// 文本分段：uid 有值 = 这一段是一个成功匹配到人的 @提及
export interface MentionSegment {
  text: string;
  uid?: number;
}

// 提及后可以紧跟的字符：标点与空白算边界，字母数字不算（避免 @ab 命中 @abc 这个人）
const BOUNDARY = /[\s,.;:!?，。；：！？、"'）)\]】]/;
// @ 前面只允许空白或开括号：否则 mail@abc.com 里的 @abc 会被当成提及
const LEADING = /[\s(（[【]/;

// candidates 名册去重并按名字长的在前排（长名优先匹配，否则 @ab 会先命中互为前缀的 @a）
function byLongestName(users: MentionUser[]): MentionUser[] {
  const seen = new Set<number>();
  const out: MentionUser[] = [];
  for (const u of users) {
    if (!u.username || seen.has(u.uid)) continue;
    seen.add(u.uid);
    out.push(u);
  }
  return out.sort((a, b) => b.username.length - a.username.length);
}

// splitMentions 把一段文本切成普通片段与提及片段，供渲染层直接 map 成节点
export function splitMentions(content: string, users: MentionUser[]): MentionSegment[] {
  const list = byLongestName(users);
  if (!list.length || !content.includes('@')) return [{ text: content }];
  const out: MentionSegment[] = [];
  let plain = '';
  let i = 0;
  while (i < content.length) {
    if (content[i] !== '@') {
      plain += content[i++];
      continue;
    }
    const prev = i > 0 ? content[i - 1] : '';
    const hit = (prev === '' || LEADING.test(prev)) &&
      list.find((u) => {
        if (!content.startsWith(u.username, i + 1)) return false;
        const next = content[i + 1 + u.username.length];
        return next === undefined || BOUNDARY.test(next);
      });
    if (!hit) {
      plain += content[i++];
      continue;
    }
    if (plain) {
      out.push({ text: plain });
      plain = '';
    }
    out.push({ text: `@${hit.username}`, uid: hit.uid });
    i += 1 + hit.username.length;
  }
  if (plain) out.push({ text: plain });
  return out.length ? out : [{ text: content }];
}

// mentionedUids 这条文本提到了谁（按 uid，绝不按用户名判定归属）
export function mentionedUids(content: string, users: MentionUser[]): number[] {
  const uids = new Set<number>();
  for (const seg of splitMentions(content, users)) {
    if (seg.uid !== undefined) uids.add(seg.uid);
  }
  return [...uids];
}

export function mentionsUser(content: string, users: MentionUser[], uid: number): boolean {
  return mentionedUids(content, users).includes(uid);
}

// ---- 输入框补全 ----

export interface MentionQuery {
  at: number; // '@' 在字符串里的下标
  query: string; // '@' 之后已经输入的部分（可能为空）
}

// mentionQuery 光标前是不是正在打一个 @提及：@ 必须在开头或紧跟空白，
// 且 @ 与光标之间不能有空白（打完空格就当这次提及结束了）
export function mentionQuery(value: string, caret: number): MentionQuery | null {
  const head = value.slice(0, caret);
  const at = head.lastIndexOf('@');
  if (at < 0) return null;
  const before = at > 0 ? head[at - 1] : '';
  if (before && !LEADING.test(before)) return null;
  const query = head.slice(at + 1);
  if (/\s/.test(query)) return null;
  return { at, query };
}

// matchMentions 按已输入的前缀筛名册（大小写不敏感，前缀命中的在前）
export function matchMentions(users: MentionUser[], query: string, limit = 6): MentionUser[] {
  const q = query.toLowerCase();
  const list = byLongestName(users).sort((a, b) => a.username.localeCompare(b.username));
  return list.filter((u) => u.username.toLowerCase().startsWith(q)).slice(0, limit);
}

// applyMention 把补全选中的名字写回输入框，返回新值与新光标位置（名字后补一个空格）
export function applyMention(value: string, q: MentionQuery, username: string): { value: string; caret: number } {
  const head = value.slice(0, q.at);
  const tail = value.slice(q.at + 1 + q.query.length);
  const inserted = `@${username} `;
  return { value: head + inserted + tail, caret: head.length + inserted.length };
}
