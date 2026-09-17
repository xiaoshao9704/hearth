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
  checkObsWsUrl,
  CONNECT_PROMPT_TIMEOUT_MS,
  CONNECT_TIMEOUT_MS,
  connectObs,
  connectTimeoutText,
  encoderLabel,
  ensureObsScene,
  listObsWindows,
  obsAudioSetupSpec,
  obsAudioSpec,
  obsCaptureToTarget,
  obsEncoderChoices,
  obsMajor,
  obsPresetVideoSettings,
  obsPlatform,
  obsVideoSpec,
  openObsInputProperties,
  setupObsCapture,
  splitObsWindowLabel,
  startObsStream,
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

test('画质预设：画布与输出一起设成同一档（只改输出等于把小画布放大，白费码率）', () => {
  assert.deepEqual(obsPresetVideoSettings(1280, 720, 60), {
    baseWidth: 1280,
    baseHeight: 720,
    outputWidth: 1280,
    outputHeight: 720,
    fpsNumerator: 60,
    fpsDenominator: 1,
  });
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

// 记下每种请求的 requestData，按请求名排队回应。
// state.inputs 是「全局源名 → kind」（OBS 里源名是全局的），state.items 是 Hearth 场景里挂着的源名。
// state.dead 列出「已被 OBS 标记移除、却还占着名字」的源：删不掉、也挂不回场景（实测的 macOS 行为）。
const NOT_FOUND = { status: { result: false, code: 600, comment: 'No source was found by the name' } };
const NAME_TAKEN = { status: { result: false, code: 601, comment: 'A source already exists by that input name.' } };
const sceneFake = (state) => {
  const inputs = { ...(state.inputs ?? {}) };
  const items = [...(state.items ?? [])];
  const dead = [...(state.dead ?? [])];
  return startFakeObs({
    reply: (d) => {
      const name = d.requestData?.inputName ?? d.requestData?.sourceName;
      if (d.requestType === 'GetSceneList') return { data: { scenes: state.scenes.map((sceneName) => ({ sceneName })) } };
      if (d.requestType === 'GetVideoSettings')
        return { data: { baseWidth: 1920, baseHeight: 1080, outputWidth: 1920, outputHeight: 1080, fpsNumerator: 60, fpsDenominator: 1 } };
      if (d.requestType === 'GetSceneItemId') return items.includes(name) ? { data: { sceneItemId: 7 } } : NOT_FOUND;
      if (d.requestType === 'GetSceneItemList') return { data: { sceneItems: items.map((sourceName) => ({ sourceName })) } };
      if (d.requestType === 'GetInputSettings')
        return inputs[name] ? { data: { inputKind: inputs[name], inputSettings: {} } } : NOT_FOUND;
      if (d.requestType === 'RemoveInput') {
        if (!inputs[name]) return NOT_FOUND;
        if (dead.includes(name)) return {}; // 删得动是假象，名字还占着
        delete inputs[name];
        const at = items.indexOf(name);
        if (at >= 0) items.splice(at, 1);
        return {};
      }
      // macOS 要从 display_capture 的默认值借主显示器 UUID，没有它源不出帧
      if (d.requestType === 'GetInputDefaultSettings')
        return { data: { defaultInputSettings: { display_uuid: 'DISPLAY-1' } } };
      if (d.requestType === 'CreateScene') return { data: { sceneUuid: 'u' } };
      if (d.requestType === 'CreateInput') {
        if (state.audioInputFails && name === 'Hearth 声音')
          return { status: { result: false, code: 604, comment: 'No such input kind' } };
        if (inputs[name]) return NAME_TAKEN;
        inputs[name] = d.requestData.inputKind;
        items.push(name);
        return { data: { inputUuid: 'u', sceneItemId: 1 } };
      }
      if (d.requestType === 'CreateSceneItem') {
        if (dead.includes(name)) return { status: { result: false, code: 700, comment: 'Failed to create the scene item.' } };
        items.push(name);
        return { data: { sceneItemId: 1 } };
      }
      if (d.requestType === 'SetInputName') {
        const to = d.requestData.newInputName;
        if (inputs[to]) return NAME_TAKEN;
        inputs[to] = inputs[name];
        delete inputs[name];
        const at = dead.indexOf(name);
        if (at >= 0) dead[at] = to;
        return {};
      }
      if (d.requestType === 'SetInputSettings' && !inputs[name]) return NOT_FOUND;
      // 属性清单：state.props 按属性名给列表项，没配的属性一律空（真 OBS 会回空列表）
      if (d.requestType === 'GetInputPropertiesListPropertyItems')
        return { data: { propertyItems: state.props?.[d.requestData.propertyName] ?? [] } };
      // 老版本 obs-websocket 没有这个请求，回 UnknownRequestType
      if (d.requestType === 'OpenInputPropertiesDialog' && state.noDialog)
        return { status: { result: false, code: 204, comment: 'Unknown request type' } };
      return {};
    },
  });
};

const sentOf = (obs, requestType) => obs.seen.requests.filter((r) => r.requestType === requestType).map((r) => r.requestData);
const orderOf = (obs) => obs.seen.requests.map((r) => r.requestType);

test('场景已存在就不重建，也不碰任何源', async () => {
  const obs = await sceneFake({ scenes: ['场景', 'Hearth 投屏'], items: ['Hearth 画面', '我的摄像头'] });
  const conn = await connectObs(obs.url, '');
  await ensureObsScene(conn);
  assert.deepEqual(sentOf(obs, 'CreateScene'), [], '场景在就不重建');
  assert.deepEqual(sentOf(obs, 'RemoveInput'), []);
  conn.close();
  await obs.close();
});

test('场景不存在就建一个', async () => {
  const obs = await sceneFake({ scenes: ['场景'] });
  const conn = await connectObs(obs.url, '');
  await ensureObsScene(conn);
  assert.deepEqual(sentOf(obs, 'CreateScene'), [{ sceneName: 'Hearth 投屏' }]);
  conn.close();
  await obs.close();
});

test('同名同 kind 的源直接复用，不删了重建（screen_capture 删完名字不会马上释放）', async () => {
  const obs = await sceneFake({
    scenes: ['Hearth 投屏'],
    items: ['Hearth 画面'],
    inputs: { 'Hearth 画面': 'screen_capture' },
  });
  const conn = await connectObs(obs.url, '');
  await setupObsCapture(conn, 'macos', 'game');
  assert.deepEqual(sentOf(obs, 'CreateInput'), [], '名字占着就不再建');
  assert.deepEqual(sentOf(obs, 'SetInputSettings'), [
    { inputName: 'Hearth 画面', inputSettings: { type: 2, display_uuid: 'DISPLAY-1' }, overlay: false },
  ]);
  assert.deepEqual(sentOf(obs, 'CreateSceneItem'), [], '已经在场景里就不重复挂');
  assert.deepEqual(
    sentOf(obs, 'RemoveInput').map((d) => d.inputName),
    ['Hearth 声音'],
    'macOS 只清掉老场景残留的声音源',
  );
  conn.close();
  await obs.close();
});

test('源还在但被挪出了场景：复用它并挂回 Hearth 场景', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], items: [], inputs: { 'Hearth 画面': 'screen_capture' } });
  const conn = await connectObs(obs.url, '');
  await setupObsCapture(conn, 'macos', 'game');
  assert.deepEqual(sentOf(obs, 'CreateSceneItem'), [{ sceneName: 'Hearth 投屏', sourceName: 'Hearth 画面' }]);
  conn.close();
  await obs.close();
});

test('名字被删不掉的死源占着：把它改名让路，再建新的', async () => {
  // macOS 实测：RemoveInput 之后 screen_capture 的名字可能一直不释放，那个源也挂不回场景。
  const obs = await sceneFake({
    scenes: ['Hearth 投屏'],
    items: [],
    inputs: { 'Hearth 画面': 'screen_capture' },
    dead: ['Hearth 画面'],
  });
  const conn = await connectObs(obs.url, '');
  await setupObsCapture(conn, 'macos', 'game');
  assert.deepEqual(sentOf(obs, 'SetInputName'), [{ inputName: 'Hearth 画面', newInputName: 'Hearth 画面-已失效1' }]);
  assert.deepEqual(
    sentOf(obs, 'CreateInput').map((d) => d.inputName),
    ['Hearth 画面', 'Hearth 画面'],
    '第一次撞 601，腾出名字后第二次建得起来',
  );
  conn.close();
  await obs.close();
});

test('换了捕获方式（kind 变了）才删了重建', async () => {
  const obs = await sceneFake({
    scenes: ['Hearth 投屏'],
    items: ['Hearth 画面'],
    inputs: { 'Hearth 画面': 'game_capture' },
  });
  const conn = await connectObs(obs.url, '');
  await setupObsCapture(conn, 'windows', 'window');
  assert.deepEqual(
    sentOf(obs, 'RemoveInput').map((d) => d.inputName),
    ['Hearth 画面'],
  );
  assert.deepEqual(
    sentOf(obs, 'CreateInput').map((d) => [d.inputName, d.inputKind]),
    [
      ['Hearth 画面', 'window_capture'],
      ['Hearth 声音', 'wasapi_process_output_capture'],
    ],
  );
  conn.close();
  await obs.close();
});

test('建采集源：macOS 只建画面一个源 → 切场景 → 弹属性窗口，顺序不能乱', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'] });
  const conn = await connectObs(obs.url, '');
  const r = await setupObsCapture(conn, 'macos', 'game');
  assert.deepEqual(r, { dialog: true, note: '' }, '画面源与属性窗口都成了就没有补充说明');
  assert.deepEqual(sentOf(obs, 'CreateInput'), [
    {
      sceneName: 'Hearth 投屏',
      inputName: 'Hearth 画面',
      inputKind: 'screen_capture',
      inputSettings: { type: 2, display_uuid: 'DISPLAY-1' },
    },
  ]);
  assert.deepEqual(sentOf(obs, 'SetCurrentProgramScene'), [{ sceneName: 'Hearth 投屏' }]);
  assert.deepEqual(sentOf(obs, 'OpenInputPropertiesDialog'), [{ inputName: 'Hearth 画面' }]);
  assert.deepEqual(orderOf(obs).slice(-3), ['RemoveInput', 'SetCurrentProgramScene', 'OpenInputPropertiesDialog']);
  assert.deepEqual(
    sentOf(obs, 'RemoveInput').map((d) => d.inputName),
    ['Hearth 声音'],
    '老场景里残留的声音源清掉，不存在（600）不算错',
  );
  // 「到 OBS 里自己选」这条路目标一概不预设，也不枚举属性清单
  assert.deepEqual(sentOf(obs, 'GetInputPropertiesListPropertyItems'), []);
  assert.deepEqual(sentOf(obs, 'SetInputSettings'), []);
  conn.close();
  await obs.close();
});

test('建采集源：Windows 的画面源按 mode 选 kind，另建一个按进程取声的声音源', async () => {
  const obs = await sceneFake({ scenes: [] });
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
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], audioInputFails: true });
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
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], noDialog: true });
  const conn = await connectObs(obs.url, '');
  const r = await setupObsCapture(conn, 'macos', 'game');
  assert.equal(r.dialog, false);
  assert.match(r.note, /双击/, '改让用户自己在 OBS 里双击那个源');
  assert.match(r.note, /Unknown request type/, '把 OBS 给的理由带出来');
  assert.equal(sentOf(obs, 'CreateInput').length, 1, '源照样建着');
  // 清残留声音源那次 RemoveInput 排在切场景之前；弹窗失败之后不能再有删源动作
  const order = orderOf(obs);
  assert.equal(order.lastIndexOf('RemoveInput') < order.indexOf('SetCurrentProgramScene'), true, '不回滚');
  assert.deepEqual(sentOf(obs, 'SetCurrentProgramScene'), [{ sceneName: 'Hearth 投屏' }]);
  conn.close();
  await obs.close();
});

test('单独弹声音源的属性窗口', async () => {
  const obs = await sceneFake({ scenes: [] });
  const conn = await connectObs(obs.url, '');
  await openObsInputProperties(conn, 'Hearth 声音');
  assert.deepEqual(sentOf(obs, 'OpenInputPropertiesDialog'), [{ inputName: 'Hearth 声音' }]);
  conn.close();
  await obs.close();
});

// ---- 从清单里选中即开播 ----
// 清单两个来源：桌面壳（bridge 的 listObsTargets）与 OBS 自己（listObsWindows，只有 macOS）。
// 属性清单请求只许打 screen_capture 的 window：application/display_uuid 的占位项是空指针，会崩 OBS。

test('画面源规格带目标时的键名（Windows 串、macOS 窗口是数字）', () => {
  const t = { kind: 'window', label: 'Game', value: '某游戏:UnrealWindow:game.exe' };
  assert.deepEqual(obsVideoSpec('windows', 'game', t).inputSettings, { capture_mode: 'window', window: t.value });
  assert.deepEqual(obsVideoSpec('windows', 'window', t).inputSettings, { method: 2, window: t.value });
  const app = obsVideoSpec('macos', 'game', { kind: 'app', label: 'Finder', value: 'com.apple.finder' });
  assert.deepEqual(app.inputSettings, { type: 2, application: 'com.apple.finder' });
  // 窗口 id 是数字：壳给什么类型就写什么类型，别把它串化
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

test('选中即开播：指到目标 → 适配画布 → 写 WHIP 配置 → 开播，全程不弹属性窗口', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'] });
  const conn = await connectObs(obs.url, '');
  // UI 就是这个顺序：选中目标先落进源，再拿本频道的地址与令牌开播
  const note = await obsCaptureToTarget(conn, 'macos', 'game', {
    kind: 'app',
    label: '访达',
    value: 'com.apple.finder',
  });
  // 目标已经替用户选好，没别的要交代
  assert.equal(note, '');
  await startObsStream(conn, 'https://h.example.com/providers/lkembed/w/7', 'tok');
  // 目标落进源 → 按画布适配场景项 → 开播前再适配一次 → 写 WHIP 配置 → 开播
  assert.deepEqual(orderOf(obs).slice(-9), [
    'SetInputSettings',
    'GetVideoSettings',
    'GetSceneItemId',
    'SetSceneItemTransform',
    'GetVideoSettings',
    'GetSceneItemId',
    'SetSceneItemTransform',
    'SetStreamServiceSettings',
    'StartStream',
  ]);
  assert.deepEqual(sentOf(obs, 'SetSceneItemTransform'), [
    {
      sceneName: 'Hearth 投屏',
      sceneItemId: 7,
      sceneItemTransform: {
        positionX: 0,
        positionY: 0,
        boundsType: 'OBS_BOUNDS_SCALE_INNER',
        boundsAlignment: 0,
        boundsWidth: 1920,
        boundsHeight: 1080,
      },
    },
    // startObsStream 自己再适配一次：浏览器那条路是在 OBS 里选完目标才回来开播的
    {
      sceneName: 'Hearth 投屏',
      sceneItemId: 7,
      sceneItemTransform: {
        positionX: 0,
        positionY: 0,
        boundsType: 'OBS_BOUNDS_SCALE_INNER',
        boundsAlignment: 0,
        boundsWidth: 1920,
        boundsHeight: 1080,
      },
    },
  ]);

  assert.deepEqual(sentOf(obs, 'SetInputSettings'), [
    // display_uuid 是 macOS 采集初始化的硬要求，从 display_capture 的默认值借来
    { inputName: 'Hearth 画面', inputSettings: { type: 2, application: 'com.apple.finder', display_uuid: 'DISPLAY-1' } },
  ]);
  assert.deepEqual(sentOf(obs, 'SetStreamServiceSettings'), [
    whipServiceSettings('https://h.example.com/providers/lkembed/w/7', 'tok'),
  ]);
  // 投什么已经定了，再把 OBS 拉到前台只会挡住人
  assert.deepEqual(sentOf(obs, 'OpenInputPropertiesDialog'), []);
  // 清单是上一步（listObsWindows）拉的，这一步不再枚举
  assert.deepEqual(sentOf(obs, 'GetInputPropertiesListPropertyItems'), []);
  conn.close();
  await obs.close();
});

test('开播前适配画布：场景/源不是本功能建的（取不到场景项）也照样开播', async () => {
  const obs = await sceneFake({ scenes: ['场景'] }); // 没有 Hearth 场景，GetSceneItemId 回 600
  const conn = await connectObs(obs.url, '');
  await startObsStream(conn, 'https://h.example.com/providers/lkembed/w/7', 'tok');
  assert.deepEqual(sentOf(obs, 'SetSceneItemTransform'), [], '适配不了就跳过');
  assert.deepEqual(orderOf(obs).slice(-2), ['SetStreamServiceSettings', 'StartStream'], '开播不受影响');
  conn.close();
  await obs.close();
});

test('壳内选中即开播：Windows 的声音源跟画面指同一个窗口', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'] });
  const conn = await connectObs(obs.url, '');
  const target = { kind: 'window', label: 'Game', value: 'a:b:game.exe' };
  await obsCaptureToTarget(conn, 'windows', 'game', target);
  assert.deepEqual(sentOf(obs, 'SetInputSettings'), [
    { inputName: 'Hearth 画面', inputSettings: { capture_mode: 'window', window: 'a:b:game.exe' } },
    { inputName: 'Hearth 声音', inputSettings: { window: 'a:b:game.exe' } },
  ]);
  conn.close();
  await obs.close();
});

test('壳内选中即开播：声音源没建成（600）不挡住开播，画面那条主路照走', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], audioInputFails: true });
  const conn = await connectObs(obs.url, '');
  const note = await obsCaptureToTarget(conn, 'windows', 'game', {
    kind: 'window',
    label: 'Game',
    value: 'a:b:game.exe',
  });
  assert.match(note, /No such input kind/, '建源那步给的说明照样带出来');
  assert.deepEqual(
    sentOf(obs, 'SetInputSettings').map((d) => d.inputName),
    ['Hearth 画面', 'Hearth 声音'],
    '声音源写不进去（600）被吞掉，没把整条路拖垮',
  );
  conn.close();
  await obs.close();
});

test('macOS 的画面源必须带 display_uuid：应用采集底层也要绑显示器', () => {
  const app = { kind: 'app', label: '访达', value: 'com.apple.finder' };
  const withUuid = obsVideoSpec('macos', 'window', app, 'UUID-1');
  assert.deepEqual(withUuid.inputSettings, { type: 2, application: 'com.apple.finder', display_uuid: 'UUID-1' });
  // 借不到 UUID 时不写这个键，而不是写空串（空串会被 OBS 当成 0 号显示器）
  assert.deepEqual(obsVideoSpec('macos', 'window', app).inputSettings, { type: 2, application: 'com.apple.finder' });
  // Windows 不受影响
  assert.equal('display_uuid' in obsVideoSpec('windows', 'game', null, 'UUID-1').inputSettings, false);
});

test('选好目标后哪个平台都不弹属性窗口：SCK 的内容列表由列窗口那一步顺带重建', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], items: [] });
  const conn = await connectObs(obs.url, '');
  const note = await obsCaptureToTarget(conn, 'macos', 'window', { kind: 'window', label: '[访达] 下载', value: 42 });
  assert.deepEqual(sentOf(obs, 'OpenInputPropertiesDialog'), []);
  assert.equal(note, '');
  const obs2 = await sceneFake({ scenes: ['Hearth 投屏'], items: [] });
  const conn2 = await connectObs(obs2.url, '');
  await obsCaptureToTarget(conn2, 'windows', 'game', { kind: 'window', label: 'x', value: 'a:b:c' });
  assert.deepEqual(sentOf(obs2, 'OpenInputPropertiesDialog'), []);
  conn.close(); await obs.close(); conn2.close(); await obs2.close();
});

test('列窗口：滤掉占位项/空名/禁用项，且只枚举 window 这一个属性', async () => {
  const obs = await sceneFake({
    scenes: ['Hearth 投屏'],
    props: {
      window: [
        { itemName: ' ', itemValue: 0, itemEnabled: true }, // OBS 给「未选择」留的占位项
        { itemName: '[OBS Studio] OBS 32.1.2 - 场景: 未命名', itemValue: 101, itemEnabled: true },
        { itemName: '', itemValue: 102, itemEnabled: true }, // 名字空的不给用户看
        { itemName: '[控制中心] Clock', itemValue: 103, itemEnabled: false }, // OBS 标了不可选
        { itemName: '[控制中心] Clock', itemValue: 104, itemEnabled: true },
        { itemName: '没有方括号前缀的窗口', itemValue: 105, itemEnabled: true },
        { itemName: '[访达] 下载', itemValue: null, itemEnabled: true }, // 值取不到
      ],
    },
  });
  const conn = await connectObs(obs.url, '');
  assert.deepEqual(await listObsWindows(conn, 'macos'), [
    { kind: 'window', label: '[OBS Studio] OBS 32.1.2 - 场景: 未命名', value: 101 },
    { kind: 'window', label: '[控制中心] Clock', value: 104 },
    { kind: 'window', label: '没有方括号前缀的窗口', value: 105 },
  ]);
  // application / display_uuid 是字符串格式的列表、占位项是空指针，枚举它们会让 OBS 段错误
  assert.deepEqual(
    sentOf(obs, 'GetInputPropertiesListPropertyItems').map((d) => d.propertyName),
    ['window'],
  );
  // 属性清单挂在源上，所以源要先建；但列个清单不该把 OBS 拉到前台、也不该切用户的场景
  assert.deepEqual(
    sentOf(obs, 'CreateInput').map((d) => d.inputName),
    ['Hearth 画面'],
  );
  assert.deepEqual(sentOf(obs, 'OpenInputPropertiesDialog'), []);
  assert.deepEqual(sentOf(obs, 'SetCurrentProgramScene'), []);
  conn.close();
  await obs.close();
});

test('列窗口：只有 macOS 走这条，别的平台返回空数组且一个请求都不发', async () => {
  const obs = await sceneFake({ scenes: ['Hearth 投屏'], props: { window: [{ itemName: 'x', itemValue: 1 }] } });
  const conn = await connectObs(obs.url, '');
  for (const plat of ['windows', 'other']) {
    assert.deepEqual(await listObsWindows(conn, plat), [], plat);
  }
  // Windows 的 window_capture/game_capture 属性清单是字符串格式的，没验过空指针占位项，一概不碰
  assert.deepEqual(orderOf(obs), []);
  conn.close();
  await obs.close();
});

test('窗口条目名拆成「应用 + 标题」：认不出前缀的原样保留，不丢条目', () => {
  assert.deepEqual(splitObsWindowLabel('[控制中心] Clock'), { app: '控制中心', title: 'Clock' });
  assert.deepEqual(splitObsWindowLabel('[OBS Studio] OBS 32.1.2 - 配置文件: hearth-test - 场景: 未命名'), {
    app: 'OBS Studio',
    title: 'OBS 32.1.2 - 配置文件: hearth-test - 场景: 未命名',
  });
  assert.deepEqual(splitObsWindowLabel('[Xcode] [main] App.swift'), { app: 'Xcode', title: '[main] App.swift' });
  assert.deepEqual(splitObsWindowLabel('[访达]'), { app: '访达', title: '无标题窗口' });
  assert.deepEqual(splitObsWindowLabel('某个窗口'), { app: '', title: '某个窗口' });
  assert.deepEqual(splitObsWindowLabel('[] 只有空括号'), { app: '', title: '[] 只有空括号' });
  assert.deepEqual(splitObsWindowLabel('com.apple.finder'), { app: '', title: 'com.apple.finder' });
});

test('编码器候选：按平台给，认不出的平台给空（界面据此只回显）', () => {
  const mac = obsEncoderChoices('macos').map((o) => o.value);
  assert.deepEqual(mac, ['apple_hevc', 'apple_h264', 'x264']);
  const win = obsEncoderChoices('windows').map((o) => o.value);
  assert.ok(win.includes('nvenc_hevc') && win.includes('qsv') && win.includes('x264'));
  assert.deepEqual(obsEncoderChoices('other'), []);
});
