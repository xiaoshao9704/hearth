// obsws 的单测：协议本身已交给 obs-websocket-js，这里只测两类东西——
// 1) 留在本模块的纯函数（地址白名单、版本号、直播服务设置、编码器名）；
// 2) 薄封装接在库上的行为（认证串、错误中文化、超时、断开回调）。
// 本机的 OBS 不能无人值守打开 obs-websocket，所以第 2 类用一个假 obs-websocket 服务器
// （ws 起的本地 WebSocket，按协议回 Hello/Identified/RequestResponse）对真库跑一遍。
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { once } from 'node:events';
import test from 'node:test';
import { WebSocketServer } from 'ws';
import {
  applyObsCapture,
  checkObsWsUrl,
  CONNECT_PROMPT_TIMEOUT_MS,
  CONNECT_TIMEOUT_MS,
  connectObs,
  connectTimeoutText,
  encoderLabel,
  ensureObsScene,
  obsAudioSetupSpec,
  obsAudioSpec,
  obsMajor,
  obsPlatform,
  obsVideoSpec,
  openObsInputProperties,
  prepareObsCapture,
  setupObsCapture,
  whipServiceSettings,
} from './tmp/obsws.js';

// obs-websocket 协议文档 Authentication 一节给出的示例 salt / challenge
const SALT = 'lM1GncleQOaCu9lT1yeUZhFYnqhsLLP1G5lAGo3ixaI=';
const CHALLENGE = '+IxH4CnCiqpX1rM9scsNynZzbOe4KhDeYcTNS3PDaeY=';
const PASSWORD = 'supersecretpassword';

const nodeAuth = (pw, salt, challenge) => {
  const sha = (s) => createHash('sha256').update(s, 'utf8').digest('base64');
  return sha(sha(pw + salt) + challenge);
};

test('地址白名单：只放行 ws://localhost、ws://127.0.0.1 与 wss://', () => {
  for (const ok of ['ws://localhost:4455', 'ws://127.0.0.1:4455', 'ws://localhost', 'wss://obs.example.com:4455']) {
    assert.equal(checkObsWsUrl(ok), '', ok);
  }
  for (const bad of ['', 'ws://192.168.1.9:4455', 'ws://obs.example.com:4455', 'http://localhost:4455', 'localhost:4455', 'ws://[::1]:4455']) {
    assert.notEqual(checkObsWsUrl(bad), '', bad);
  }
  assert.equal(checkObsWsUrl('ws://192.168.1.20:4455', true), '');
  assert.notEqual(checkObsWsUrl('ws://192.168.1.20:4455', false), '');
  assert.notEqual(checkObsWsUrl('ws://8.8.8.8:4455', true), '');
});

test('主版本号解析：取不到时给 0（不据此判不支持）', () => {
  assert.equal(obsMajor('30.2.3'), 30);
  assert.equal(obsMajor('29.1.3'), 29);
  assert.equal(obsMajor('31.0.0-beta1'), 31);
  assert.equal(obsMajor(''), 0);
  assert.equal(obsMajor('x.y.z'), 0);
});

test('直播服务设置就是 whip_custom + server + bearer_token', () => {
  assert.deepEqual(whipServiceSettings('https://h.example.com/providers/lkembed/w/7', 'tok'), {
    streamServiceType: 'whip_custom',
    streamServiceSettings: { server: 'https://h.example.com/providers/lkembed/w/7', bearer_token: 'tok' },
  });
});

test('编码器名：常见 id 翻成人话，认不出的原样显示', () => {
  assert.match(encoderLabel('x264'), /x264/);
  assert.match(encoderLabel('jim_nvenc'), /NVENC/);
  assert.match(encoderLabel('nvenc_hevc'), /NVENC/);
  assert.match(encoderLabel('amd'), /AMD/);
  assert.match(encoderLabel('jim_hevc_amf'), /AMD/);
  assert.match(encoderLabel('obs_qsv11'), /QSV/);
  assert.match(encoderLabel('apple_h264'), /VideoToolbox/);
  assert.match(encoderLabel('com.apple.videotoolbox.videoencoder.ave.avc'), /VideoToolbox/);
  assert.equal(encoderLabel('some_future_encoder'), 'some_future_encoder');
  assert.equal(encoderLabel('  '), '');
});

// ---- 假 obs-websocket 服务器：够真库走完握手与请求-响应 ----
const startFakeObs = async (opts = {}) => {
  const wss = new WebSocketServer({
    host: '127.0.0.1',
    port: 0,
    handleProtocols: (protocols) => (protocols.has('obswebsocket.json') ? 'obswebsocket.json' : false),
  });
  await once(wss, 'listening');
  const seen = { identify: null, requests: [], closed: false };
  let sock = null;
  wss.on('connection', (s) => {
    sock = s;
    s.on('close', () => {
      seen.closed = true; // 客户端有没有主动断开：权限提示挂着时不能断
    });
    if (opts.silent) return; // 不回 Hello：用来验超时
    s.send(JSON.stringify({ op: 0, d: { rpcVersion: 1, ...(opts.auth ? { authentication: { challenge: CHALLENGE, salt: SALT } } : {}) } }));
    s.on('message', (raw) => {
      const msg = JSON.parse(raw.toString());
      if (msg.op === 1) {
        seen.identify = msg.d;
        if (opts.rejectAuth) s.close(4009, 'Authentication failed');
        else s.send(JSON.stringify({ op: 2, d: { negotiatedRpcVersion: 1 } }));
        return;
      }
      if (msg.op !== 6) return;
      seen.requests.push(msg.d);
      const reply = opts.reply?.(msg.d);
      if (reply === null) return; // 故意不回：验请求侧的兜底
      s.send(
        JSON.stringify({
          op: 7,
          d: {
            requestType: msg.d.requestType,
            requestId: msg.d.requestId,
            requestStatus: reply?.status ?? { result: true, code: 100 },
            responseData: reply?.data ?? {},
          },
        }),
      );
    });
  });
  return {
    url: `ws://127.0.0.1:${wss.address().port}`,
    seen,
    kill: () => sock?.terminate(), // 直接掐断，客户端看到的就是 1006
    close: () =>
      new Promise((r) => {
        for (const c of wss.clients) c.terminate();
        wss.close(r);
      }),
  };
};

test('握手：库按协议算出认证串，并带上我们要的 rpcVersion', async () => {
  const obs = await startFakeObs({ auth: true });
  const conn = await connectObs(obs.url, PASSWORD);
  assert.equal(obs.seen.identify.rpcVersion, 1);
  assert.equal(obs.seen.identify.authentication, nodeAuth(PASSWORD, SALT, CHALLENGE), '与 node:crypto 独立算出的值一致');
  assert.equal(conn.alive, true);
  conn.close();
  await obs.close();
});

test('握手：OBS 要鉴权但没填密码，4009 的文案指向「请填写密码」', async () => {
  const obs = await startFakeObs({ auth: true, rejectAuth: true });
  await assert.rejects(connectObs(obs.url, ''), /请填写密码/);
  await obs.close();
});

test('握手：填了密码仍被 4009 拒，文案是「密码不对」', async () => {
  const obs = await startFakeObs({ auth: true, rejectAuth: true });
  await assert.rejects(connectObs(obs.url, 'wrong'), /密码不对/);
  await obs.close();
});

test('握手超时：服务器不回 Hello 时到点 reject', async () => {
  const obs = await startFakeObs({ silent: true });
  await assert.rejects(connectObs(obs.url, '', false, { timeoutMs: 120 }), /超时/);
  await obs.close();
});

// Chrome 142+ 的 Local Network Access：没批权限时 WebSocket 一直 CONNECTING，最后表现成我们的超时。
// 浏览器没有的东西在 node 里靠桩：navigator.permissions.query 三种结果各验一次文案。
const withNavigator = async (stub, fn) => {
  const had = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  Object.defineProperty(globalThis, 'navigator', { value: stub, configurable: true, writable: true });
  try {
    await fn();
  } finally {
    if (had) Object.defineProperty(globalThis, 'navigator', had);
    else delete globalThis.navigator;
  }
};
const navStub = (state) => ({
  permissions: {
    query: async ({ name }) => {
      assert.equal(name, 'local-network-access', '只问这一个权限');
      if (state === 'throw') throw new TypeError(`Unknown permission name: ${name}`); // 老浏览器不认识这个名字
      return { state };
    },
  },
});

test('握手超时：本地网络权限还没批（prompt）时，文案指向 Chrome 的允许提示', async () => {
  const obs = await startFakeObs({ silent: true });
  await withNavigator(navStub('prompt'), async () => {
    await assert.rejects(connectObs(obs.url, '', false, { timeoutMs: 120, promptTimeoutMs: 200 }), (e) => {
      assert.match(e.message, /本地网络/);
      assert.match(e.message, /允许/);
      assert.doesNotMatch(e.message, /WebSocket 服务器设置/, '别再把人引去查 OBS 的设置');
      return true;
    });
  });
  await obs.close();
});

// 权限提示是「我们一断开它就没了」：常规超时到点就断，用户照着文案去点「允许」时提示已经消失。
// 所以 prompt 态要挂住这条连接（默认 60 秒），并先通知 UI 说清楚在等什么。
// 下面两条把两档超时缩小成毫秒级跑，真值另有一条断言兜着。
const stillPending = async (p, ms) => {
  const tick = Symbol('tick');
  const r = await Promise.race([
    p.then(
      () => 'resolved',
      () => 'rejected',
    ),
    new Promise((ok) => setTimeout(() => ok(tick), ms)),
  ]);
  return r === tick;
};

test('本地网络权限还没批：常规超时到点不断开，先回调通知 UI，最后仍报 prompt 文案', async () => {
  const obs = await startFakeObs({ silent: true });
  await withNavigator(navStub('prompt'), async () => {
    const waits = [];
    const p = connectObs(obs.url, '', false, {
      timeoutMs: 100,
      promptTimeoutMs: 1200,
      onWaiting: (why) => waits.push(why),
    });
    assert.equal(await stillPending(p, 400), true, '常规超时（100ms）的四倍时间过去了也不能拒');
    assert.deepEqual(waits, ['local-network'], 'UI 收到过一次「在等本地网络权限」');
    assert.equal(obs.seen.closed, false, '在途连接没被我们断开，浏览器的权限提示才挂得住');
    await assert.rejects(p, /本地网络/);
  });
  await obs.close();
});

test('本地网络权限已批（granted）：仍走常规超时，不白等宽限', async () => {
  const obs = await startFakeObs({ silent: true });
  await withNavigator(navStub('granted'), async () => {
    const t = Date.now();
    await assert.rejects(connectObs(obs.url, '', false, { timeoutMs: 100, promptTimeoutMs: 5000 }), /WebSocket 服务器设置/);
    assert.ok(Date.now() - t < 2000, `按常规超时拒掉，实测 ${Date.now() - t}ms`);
  });
  await obs.close();
});

test('两档超时的默认值：常规 8 秒，等权限时至少 60 秒', () => {
  assert.equal(CONNECT_TIMEOUT_MS, 8000);
  assert.ok(CONNECT_PROMPT_TIMEOUT_MS >= 60000);
});

test('握手超时：本地网络权限被拒（denied）时，文案指向站点设置', async () => {
  const obs = await startFakeObs({ silent: true });
  await withNavigator(navStub('denied'), async () => {
    await assert.rejects(connectObs(obs.url, '', false, { timeoutMs: 120 }), (e) => {
      assert.match(e.message, /已拒绝/);
      assert.match(e.message, /站点设置/);
      return true;
    });
  });
  await obs.close();
});

test('握手超时：浏览器不认识这个权限名（query 抛）时，保留原来的 OBS 文案', async () => {
  const obs = await startFakeObs({ silent: true });
  await withNavigator(navStub('throw'), async () => {
    await assert.rejects(connectObs(obs.url, '', false, { timeoutMs: 120 }), /WebSocket 服务器设置/);
  });
  await obs.close();
});

test('超时文案：granted 与探测不出来时都是原来那句', () => {
  assert.match(connectTimeoutText('granted'), /WebSocket 服务器设置/);
  assert.match(connectTimeoutText('unknown'), /WebSocket 服务器设置/);
});

test('地址不合法时根本不建连', async () => {
  await assert.rejects(connectObs('ws://192.168.1.9:4455', ''), /只允许连本机/);
  await assert.rejects(connectObs('ws://192.168.1.9:4455', '', false), /只允许连本机/);
});

test('请求：请求形状原样送到，响应原样取回', async () => {
  const obs = await startFakeObs({
    reply: (d) => (d.requestType === 'GetVersion' ? { data: { obsVersion: '32.1.2', obsWebSocketVersion: '5.7.3' } } : {}),
  });
  const conn = await connectObs(obs.url, '');
  assert.equal((await conn.request('GetVersion')).obsVersion, '32.1.2');
  await conn.request('SetStreamServiceSettings', whipServiceSettings('https://h.example.com/w/7', 'tok'));
  const sent = obs.seen.requests.at(-1);
  assert.equal(sent.requestType, 'SetStreamServiceSettings');
  assert.deepEqual(sent.requestData, whipServiceSettings('https://h.example.com/w/7', 'tok'));
  conn.close();
  await obs.close();
});

test('请求失败：requestStatus 的 code 与 comment 带进中文错误', async () => {
  const obs = await startFakeObs({ reply: () => ({ status: { result: false, code: 500, comment: 'Output already active' } }) });
  const conn = await connectObs(obs.url, '');
  await assert.rejects(conn.request('StartStream'), (e) => {
    assert.match(e.message, /StartStream/);
    assert.match(e.message, /500/);
    assert.match(e.message, /Output already active/);
    return true;
  });
  conn.close();
  await obs.close();
});

test('中途断开：在途请求被拒，onLost 触发一次，连接转为不可用', async () => {
  const obs = await startFakeObs({ reply: () => null });
  const conn = await connectObs(obs.url, '');
  let lost = 0;
  conn.onLost = () => lost++;
  const p = conn.request('GetStreamStatus');
  obs.kill();
  await assert.rejects(p, /OBS/);
  assert.equal(conn.alive, false);
  assert.equal(lost, 1, 'onLost 只触发一次');
  await assert.rejects(conn.request('GetVersion'), /OBS/);
  await obs.close();
});

test('主动 close 不触发 onLost，之后的请求直接拒', async () => {
  const obs = await startFakeObs();
  const conn = await connectObs(obs.url, '');
  let lost = 0;
  conn.onLost = () => lost++;
  conn.close();
  assert.equal(conn.alive, false);
  assert.equal(lost, 0);
  await assert.rejects(conn.request('GetVersion'), /已关闭/);
  await obs.close();
});


// ---- 在 OBS 里建采集源 ----

test('平台识别：认不出的一概当 other（功能不可用，而不是当成 Windows 乱建源）', () => {
  assert.equal(obsPlatform('windows'), 'windows');
  assert.equal(obsPlatform('macos'), 'macos');
  assert.equal(obsPlatform('MacOS'), 'macos');
  assert.equal(obsPlatform('ubuntu'), 'other');
  assert.equal(obsPlatform(undefined), 'other');
});

test('画面源规格：不预设目标，Windows 两种捕获方式只差 kind 与键名', () => {
  const game = obsVideoSpec('windows', 'game', null);
  assert.equal(game.inputKind, 'game_capture');
  assert.deepEqual(game.inputSettings, { capture_mode: 'window' });
  const win = obsVideoSpec('windows', 'window', null);
  assert.equal(win.inputKind, 'window_capture');
  assert.deepEqual(win.inputSettings, { method: 2 }); // 2 = WGC
});

test('画面源规格：macOS 是 screen_capture + type 2（应用），不带 display_uuid', () => {
  const mac = obsVideoSpec('macos', 'game', null);
  assert.equal(mac.inputKind, 'screen_capture');
  assert.deepEqual(mac.inputSettings, { type: 2 });
});

test('声音源规格：只有 Windows 要单独的声音源，macOS 的画面源自带应用声音', () => {
  assert.deepEqual(obsAudioSetupSpec('windows'), {
    inputKind: 'wasapi_process_output_capture',
    inputSettings: {},
  });
  assert.equal(obsAudioSetupSpec('macos'), null, 'screen_capture 固定连声音一起采，再建一个就是采两遍');
  assert.equal(obsAudioSetupSpec('other'), null);
});

// 记下每种请求的 requestData，按请求名排队回应
const sceneFake = (state) =>
  startFakeObs({
    reply: (d) => {
      if (d.requestType === 'GetSceneList') return { data: { scenes: state.scenes.map((sceneName) => ({ sceneName })) } };
      if (d.requestType === 'GetSceneItemList') return { data: { sceneItems: state.items.map((sourceName) => ({ sourceName })) } };
      if (d.requestType === 'GetInputPropertiesListPropertyItems')
        return { data: { propertyItems: state.props?.[d.requestData.propertyName] ?? [] } };
      if (d.requestType === 'GetInputDefaultSettings') return { data: { defaultInputSettings: { display_uuid: 'DISPLAY-1' } } };
      if (d.requestType === 'RemoveInput')
        return state.items.includes(d.requestData.inputName)
          ? {}
          : { status: { result: false, code: 600, comment: 'No source was found by the name' } };
      if (d.requestType === 'CreateScene') return { data: { sceneUuid: 'u' } };
      if (d.requestType === 'CreateInput') {
        if (state.audioInputFails && d.requestData.inputName === 'Hearth 声音')
          return { status: { result: false, code: 604, comment: 'No such input kind' } };
        return { data: { inputUuid: 'u', sceneItemId: 1 } };
      }
      // 老版本 obs-websocket 没有这个请求，回 UnknownRequestType
      if (d.requestType === 'OpenInputPropertiesDialog' && state.noDialog)
        return { status: { result: false, code: 204, comment: 'Unknown request type' } };
      return {};
    },
  });

const sentOf = (obs, requestType) => obs.seen.requests.filter((r) => r.requestType === requestType).map((r) => r.requestData);
const orderOf = (obs) => obs.seen.requests.map((r) => r.requestType);

test('场景已存在时只清掉本功能的两个源，用户自己加的源不碰', async () => {
  const obs = await sceneFake({ scenes: ['场景', 'Hearth 投屏'], items: ['Hearth 画面', 'Hearth 声音', '我的摄像头'] });
  const conn = await connectObs(obs.url, '');
  await ensureObsScene(conn);
  assert.deepEqual(sentOf(obs, 'CreateScene'), [], '场景在就不重建');
  assert.deepEqual(
    sentOf(obs, 'RemoveInput').map((d) => d.inputName),
    ['Hearth 画面', 'Hearth 声音'],
  );
  conn.close();
  await obs.close();
});

test('场景不存在就建一个；两个固定名源按名清一遍，不存在（600）不算错', async () => {
  const obs = await sceneFake({ scenes: ['场景'], items: [] });
  const conn = await connectObs(obs.url, '');
  await ensureObsScene(conn);
  assert.deepEqual(sentOf(obs, 'CreateScene'), [{ sceneName: 'Hearth 投屏' }]);
  assert.deepEqual(
    sentOf(obs, 'RemoveInput').map((d) => d.inputName),
    ['Hearth 画面', 'Hearth 声音'],
  );
  conn.close();
  await obs.close();
});

test('建采集源：macOS 只建画面一个源 → 切场景 → 弹属性窗口，顺序不能乱', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], items: [] });
  const conn = await connectObs(obs.url, '');
  const r = await setupObsCapture(conn, 'macos', 'game');
  assert.deepEqual(r, { dialog: true, note: '' }, '画面源与属性窗口都成了就没有补充说明');
  assert.deepEqual(sentOf(obs, 'CreateInput'), [
    { sceneName: 'Hearth 投屏', inputName: 'Hearth 画面', inputKind: 'screen_capture', inputSettings: { type: 2 } },
  ]);
  assert.deepEqual(sentOf(obs, 'SetCurrentProgramScene'), [{ sceneName: 'Hearth 投屏' }]);
  assert.deepEqual(sentOf(obs, 'OpenInputPropertiesDialog'), [{ inputName: 'Hearth 画面' }]);
  assert.deepEqual(orderOf(obs).slice(-3), ['CreateInput', 'SetCurrentProgramScene', 'OpenInputPropertiesDialog']);
  // 目标一概不预设：浏览器里 hearth 不枚举窗口清单（那会把 OBS 带走），也就没有 SetInputSettings
  assert.deepEqual(sentOf(obs, 'GetInputPropertiesListPropertyItems'), []);
  assert.deepEqual(sentOf(obs, 'SetInputSettings'), []);
  conn.close();
  await obs.close();
});

test('建采集源：Windows 的画面源按 mode 选 kind，另建一个按进程取声的声音源', async () => {
  const obs = await sceneFake({ scenes: [], items: [] });
  const conn = await connectObs(obs.url, '');
  await setupObsCapture(conn, 'windows', 'window');
  assert.deepEqual(sentOf(obs, 'CreateInput'), [
    { sceneName: 'Hearth 投屏', inputName: 'Hearth 画面', inputKind: 'window_capture', inputSettings: { method: 2 } },
    { sceneName: 'Hearth 投屏', inputName: 'Hearth 声音', inputKind: 'wasapi_process_output_capture', inputSettings: {} },
  ]);
  conn.close();
  await obs.close();
});

test('声音源建不出来时照样切场景、照样弹窗，理由带 OBS 的 comment 回给用户', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], items: [], audioInputFails: true });
  const conn = await connectObs(obs.url, '');
  const r = await setupObsCapture(conn, 'windows', 'game');
  assert.equal(r.dialog, true);
  assert.match(r.note, /No such input kind/);
  assert.deepEqual(sentOf(obs, 'SetCurrentProgramScene'), [{ sceneName: 'Hearth 投屏' }], '画面那条主路不被声音挡住');
  assert.deepEqual(sentOf(obs, 'OpenInputPropertiesDialog'), [{ inputName: 'Hearth 画面' }]);
  conn.close();
  await obs.close();
});

test('老版本 obs-websocket 弹不出属性窗口：给中文说明，已建好的源不回滚', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], items: [], noDialog: true });
  const conn = await connectObs(obs.url, '');
  const r = await setupObsCapture(conn, 'macos', 'game');
  assert.equal(r.dialog, false);
  assert.match(r.note, /双击/, '改让用户自己在 OBS 里双击那个源');
  assert.match(r.note, /Unknown request type/, '把 OBS 给的理由带出来');
  assert.equal(sentOf(obs, 'CreateInput').length, 1, '源照样建着');
  // 开头按名清理会先发两次 RemoveInput；弹窗失败之后不能再有删源动作
  const order = orderOf(obs);
  assert.equal(order.lastIndexOf('RemoveInput') < order.indexOf('CreateInput'), true, '不回滚');
  assert.deepEqual(sentOf(obs, 'SetCurrentProgramScene'), [{ sceneName: 'Hearth 投屏' }]);
  conn.close();
  await obs.close();
});

test('单独弹声音源的属性窗口', async () => {
  const obs = await sceneFake({ scenes: [], items: [] });
  const conn = await connectObs(obs.url, '');
  await openObsInputProperties(conn, 'Hearth 声音');
  assert.deepEqual(sentOf(obs, 'OpenInputPropertiesDialog'), [{ inputName: 'Hearth 声音' }]);
  conn.close();
  await obs.close();
});

// ---- 枚举目标（未启用）----
// UI 不再调用这条路（枚举 macOS screen_capture 会让 OBS 段错误），但代码还在，测试跟着留。

test('未启用：画面源规格带目标时的键名（Windows 串、macOS 窗口是数字）', () => {
  const t = { kind: 'window', label: 'Game', value: '某游戏:UnrealWindow:game.exe' };
  assert.deepEqual(obsVideoSpec('windows', 'game', t).inputSettings, { capture_mode: 'window', window: t.value });
  assert.deepEqual(obsVideoSpec('windows', 'window', t).inputSettings, { method: 2, window: t.value });
  const app = obsVideoSpec('macos', 'game', { kind: 'app', label: 'Finder', value: 'com.apple.finder' });
  assert.deepEqual(app.inputSettings, { type: 2, application: 'com.apple.finder' });
  // 窗口 id 是数字：属性列表给什么类型就回什么类型，别把它串化
  assert.deepEqual(obsVideoSpec('macos', 'game', { kind: 'window', label: '某窗口', value: 4242 }).inputSettings, {
    type: 1,
    window: 4242,
  });
});

test('声音源按目标取声：只有 Windows 有这么一个源', () => {
  const winTarget = { kind: 'window', label: 'Game', value: '某游戏:UnrealWindow:game.exe' };
  assert.deepEqual(obsAudioSpec('windows', winTarget), {
    inputKind: 'wasapi_process_output_capture',
    inputSettings: { window: winTarget.value },
  });
  assert.equal(obsAudioSpec('macos', { kind: 'app', label: 'Finder', value: 'com.apple.finder' }), null);
  assert.equal(obsAudioSpec('macos', { kind: 'window', label: '某窗口', value: 7 }), null);
  assert.equal(obsAudioSpec('other', { kind: 'app', label: 'x', value: 'x' }), null);
});

test('未启用：取清单只留 enabled 的项，空占位项丢掉，显示用 itemName', async () => {
  const obs = await sceneFake({
    scenes: [],
    items: [],
    props: {
      application: [
        { itemName: '', itemValue: '', itemEnabled: true },
        { itemName: '访达', itemValue: 'com.apple.finder', itemEnabled: true },
        { itemName: '关掉的', itemValue: 'com.x.y', itemEnabled: false },
      ],
      window: [
        { itemName: '某窗口', itemValue: 4242, itemEnabled: true },
        { itemName: '占位', itemValue: 0, itemEnabled: true },
      ],
    },
  });
  const conn = await connectObs(obs.url, '');
  assert.deepEqual(await prepareObsCapture(conn, 'macos', 'game'), [
    { kind: 'app', value: 'com.apple.finder', label: '访达' },
    { kind: 'window', value: 4242, label: '某窗口' },
  ]);
  conn.close();
  await obs.close();
});

test('未启用：确认选择时写画面源 → 建声音源 → 切场景', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], items: [] });
  const conn = await connectObs(obs.url, '');
  const note = await applyObsCapture(conn, 'windows', 'game', { kind: 'window', label: 'Game', value: 'a:b:game.exe' });
  assert.equal(note, '');
  assert.deepEqual(sentOf(obs, 'SetInputSettings'), [
    { inputName: 'Hearth 画面', inputSettings: { capture_mode: 'window', window: 'a:b:game.exe' } },
  ]);
  assert.equal(obs.seen.requests.at(-1).requestType, 'SetCurrentProgramScene');
  conn.close();
  await obs.close();
});
