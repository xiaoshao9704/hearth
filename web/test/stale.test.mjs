// 升级后还开着的旧页面：引擎 chunk 已随新产物换名，动态导入必 404。
// 这里测的是「认得出这种错」与「一次会话只自动刷新一次」——房间页据此不再无限重连。
import assert from 'node:assert/strict';
import test from 'node:test';
import { isStaleChunkError, reloadForStale, staleReloadUsed } from './tmp/stale.js';

const memStore = () => {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
  };
};

// sessionStorage 与 location 在 node 里没有，按用例打桩；reload 只记次数不真刷
const withGlobals = (storage, fn) => {
  let reloads = 0;
  const stubs = { sessionStorage: storage, location: { reload: () => reloads++ } };
  const had = Object.keys(stubs).map((k) => [k, Object.getOwnPropertyDescriptor(globalThis, k)]);
  for (const [k, v] of Object.entries(stubs)) {
    Object.defineProperty(globalThis, k, { value: v, configurable: true, writable: true });
  }
  try {
    return fn(() => reloads);
  } finally {
    for (const [k, desc] of had) {
      if (desc) Object.defineProperty(globalThis, k, desc);
      else delete globalThis[k];
    }
  }
};

test('认得出各家浏览器的「动态导入的模块拉不回来」', () => {
  for (const msg of [
    'Failed to fetch dynamically imported module: https://hearth.example.com/assets/livekit-abc123.js',
    'error loading dynamically imported module: https://hearth.example.com/assets/livekit-abc123.js',
    'Importing a module script failed.',
  ]) {
    assert.equal(isStaleChunkError(new Error(msg)), true, msg);
  }
  const named = new Error('Loading chunk 7 failed.');
  named.name = 'ChunkLoadError';
  assert.equal(isStaleChunkError(named), true, '打包器自己的错误名');
});

test('别的失败一概不算旧产物：重连该照常退避重试', () => {
  for (const e of [
    new Error('连不上服务器'),
    new Error('WebSocket connection failed'),
    new Error('signal connection error: 503'),
    null,
    undefined,
    'Failed to fetch',
  ]) {
    assert.equal(isStaleChunkError(e), false, String(e));
  }
});

test('一次会话只自动刷新一次，第二次交给调用方提示用户', () => {
  withGlobals(memStore(), (reloads) => {
    assert.equal(staleReloadUsed(), false);
    assert.equal(reloadForStale(), true);
    assert.equal(reloads(), 1);
    assert.equal(staleReloadUsed(), true, '标记落下了，再遇到就不刷');
  });
});

test('标记落不下时不刷：宁可提示手动刷新，也不能刷个没完', () => {
  const throwing = {
    getItem: () => {
      throw new Error('隐私模式');
    },
    setItem: () => {
      throw new Error('隐私模式');
    },
  };
  withGlobals(throwing, (reloads) => {
    assert.equal(staleReloadUsed(), true, '读不到标记就当刷过');
    assert.equal(reloadForStale(), false);
    assert.equal(reloads(), 0);
  });
  // 有的浏览器不抛错，只是写了读不回来
  const dropping = { getItem: () => null, setItem: () => {} };
  withGlobals(dropping, (reloads) => {
    assert.equal(reloadForStale(), false);
    assert.equal(reloads(), 0);
  });
});
