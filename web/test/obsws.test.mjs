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
import { checkObsWsUrl, connectObs, encoderLabel, obsMajor, whipServiceSettings } from './tmp/obsws.js';

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
  const seen = { identify: null, requests: [] };
  let sock = null;
  wss.on('connection', (s) => {
    sock = s;
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
  await assert.rejects(connectObs(obs.url, '', false, 120), /超时/);
  await obs.close();
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
