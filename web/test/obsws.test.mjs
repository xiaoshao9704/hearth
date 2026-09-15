// obs-websocket 5.x 客户端的单测：认证串与握手/请求状态机都是纯逻辑，用假 socket 直接喂帧。
// 协议文档只给了 salt/challenge 的示例值、没给算完的认证串，所以期望值由 node:crypto 独立算一遍
// 再与写死的黄金值对照——两边同时算错才可能漏过。
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import test from 'node:test';
import { ObsConn, checkObsWsUrl, connectObs, obsAuthString, obsMajor, whipServiceSettings } from './tmp/obsws.js';

// obs-websocket 协议文档 Authentication 一节给出的示例 salt / challenge
const SALT = 'lM1GncleQOaCu9lT1yeUZhFYnqhsLLP1G5lAGo3ixaI=';
const CHALLENGE = '+IxH4CnCiqpX1rM9scsNynZzbOe4KhDeYcTNS3PDaeY=';
const PASSWORD = 'supersecretpassword';

const nodeAuth = (pw, salt, challenge) => {
  const sha = (s) => createHash('sha256').update(s, 'utf8').digest('base64');
  return sha(sha(pw + salt) + challenge);
};

test('认证串 = base64(sha256(base64(sha256(密码+salt)) + challenge))', async () => {
  const want = '1Ct943GAT+6YQUUX47Ia/ncufilbe6+oD6lY+5kaCu4=';
  assert.equal(nodeAuth(PASSWORD, SALT, CHALLENGE), want, 'node:crypto 侧的黄金值');
  assert.equal(await obsAuthString(PASSWORD, SALT, CHALLENGE), want);
});

test('认证串：空密码与非 ASCII 密码也与 node:crypto 一致', async () => {
  for (const pw of ['', 'ķŋ密码🔥', 'a'.repeat(200)]) {
    assert.equal(await obsAuthString(pw, SALT, CHALLENGE), nodeAuth(pw, SALT, CHALLENGE), `密码 ${JSON.stringify(pw)}`);
  }
});

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
  {
  }
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

// ---- 假 socket：记录发出的帧，手动喂进来的帧当作 OBS 的回复 ----
class FakeSocket {
  constructor() {
    this.sent = [];
    this.closed = false;
    this.onopen = null;
    this.onclose = null;
    this.onerror = null;
    this.onmessage = null;
  }
  send(text) {
    if (this.closed) throw new Error('socket closed');
    this.sent.push(JSON.parse(text));
  }
  close() {
    this.closed = true;
  }
  feed(msg) {
    this.onmessage?.(JSON.stringify(msg));
  }
  // 等到第 n 帧发出（onFrame 是 async，Hello 之后的 Identify 要过一轮微任务）
  async waitSent(n) {
    for (let i = 0; i < 200 && this.sent.length < n; i++) await new Promise((r) => setImmediate(r));
    assert.ok(this.sent.length >= n, `期望至少发出 ${n} 帧，实际 ${this.sent.length}`);
    return this.sent[n - 1];
  }
}

const hello = (auth) => ({ op: 0, d: { rpcVersion: 1, ...(auth ? { authentication: { challenge: CHALLENGE, salt: SALT } } : {}) } });
const identified = { op: 2, d: { negotiatedRpcVersion: 1 } };

const withFake = () => {
  const sock = new FakeSocket();
  return { sock, factory: () => sock };
};

test('握手：无鉴权时 Identify 只带 rpcVersion', async () => {
  const { sock, factory } = withFake();
  const p = connectObs('ws://localhost:4455', '', factory, 2000);
  sock.feed(hello(false));
  const id = await sock.waitSent(1);
  assert.deepEqual(id, { op: 1, d: { rpcVersion: 1 } });
  sock.feed(identified);
  const conn = await p;
  assert.equal(conn.alive, true);
  conn.close();
});

test('握手：有鉴权时 Identify 带上算好的认证串', async () => {
  const { sock, factory } = withFake();
  const p = connectObs('ws://localhost:4455', PASSWORD, factory, 2000);
  sock.feed(hello(true));
  const id = await sock.waitSent(1);
  assert.equal(id.op, 1);
  assert.equal(id.d.rpcVersion, 1);
  assert.equal(id.d.authentication, nodeAuth(PASSWORD, SALT, CHALLENGE));
  sock.feed(identified);
  await p;
});

test('握手：OBS 要鉴权但没填密码，直接给出中文提示且不发认证串', async () => {
  const { sock, factory } = withFake();
  const p = connectObs('ws://localhost:4455', '', factory, 2000);
  sock.feed(hello(true));
  await assert.rejects(p, /请填写密码/);
  assert.deepEqual(sock.sent, []);
});

test('握手：密码不对时 OBS 关连接（4009），错误说明是密码不对', async () => {
  const { sock, factory } = withFake();
  const p = connectObs('ws://localhost:4455', 'wrong', factory, 2000);
  sock.feed(hello(true));
  await sock.waitSent(1);
  sock.onclose(4009, '');
  await assert.rejects(p, /密码不对/);
});

test('握手超时：到点 reject 并关掉 socket', async () => {
  const { sock, factory } = withFake();
  await assert.rejects(connectObs('ws://localhost:4455', '', factory, 30), /超时/);
  assert.equal(sock.closed, true);
});

test('地址不合法时根本不建连', async () => {
  let made = 0;
  await assert.rejects(
    connectObs('ws://192.168.1.9:4455', '', () => {
      made++;
      return new FakeSocket();
    }),
    /只允许连本机/,
  );
  assert.equal(made, 0);
});

// 建好一条已握手的连接，供请求-响应用例复用
const connected = async () => {
  const { sock, factory } = withFake();
  const p = connectObs('ws://localhost:4455', '', factory, 2000);
  sock.feed(hello(false));
  await sock.waitSent(1);
  sock.feed(identified);
  return { sock, conn: await p };
};

test('请求：op 6 带 requestType/requestId/requestData，op 7 按 requestId 配对', async () => {
  const { sock, conn } = await connected();
  const a = conn.request('GetVersion');
  const b = conn.request('SetStreamServiceSettings', whipServiceSettings('https://h.example.com/w/7', 'tok'));
  const f1 = await sock.waitSent(2);
  const f2 = await sock.waitSent(3);
  assert.equal(f1.op, 6);
  assert.equal(f1.d.requestType, 'GetVersion');
  assert.equal(f2.d.requestType, 'SetStreamServiceSettings');
  assert.deepEqual(f2.d.requestData, whipServiceSettings('https://h.example.com/w/7', 'tok'));
  assert.notEqual(f1.d.requestId, f2.d.requestId, 'requestId 必须逐条不同');

  // 乱序回，仍按 id 配对
  sock.feed({ op: 7, d: { requestType: 'SetStreamServiceSettings', requestId: f2.d.requestId, requestStatus: { result: true, code: 100 } } });
  sock.feed({
    op: 7,
    d: { requestType: 'GetVersion', requestId: f1.d.requestId, requestStatus: { result: true, code: 100 }, responseData: { obsVersion: '30.2.3' } },
  });
  assert.deepEqual(await b, {});
  assert.equal((await a).obsVersion, '30.2.3');
  conn.close();
});

test('请求失败：requestStatus.comment 带进中文错误', async () => {
  const { sock, conn } = await connected();
  const p = conn.request('StartStream');
  const f = await sock.waitSent(2);
  sock.feed({
    op: 7,
    d: { requestType: 'StartStream', requestId: f.d.requestId, requestStatus: { result: false, code: 500, comment: 'Output already active' } },
  });
  await assert.rejects(p, (e) => {
    assert.match(e.message, /StartStream/);
    assert.match(e.message, /500/);
    assert.match(e.message, /Output already active/);
    return true;
  });
  conn.close();
});

test('事件帧（op 5）与陌生 requestId 一律丢弃，不影响在途请求', async () => {
  const { sock, conn } = await connected();
  const p = conn.request('GetStreamStatus');
  const f = await sock.waitSent(2);
  sock.feed({ op: 5, d: { eventType: 'StreamStateChanged' } });
  sock.feed({ op: 7, d: { requestType: 'X', requestId: 'not-mine', requestStatus: { result: true } } });
  sock.onmessage('这不是 JSON');
  sock.feed({ op: 7, d: { requestType: 'GetStreamStatus', requestId: f.d.requestId, requestStatus: { result: true }, responseData: { outputActive: true } } });
  assert.equal((await p).outputActive, true);
  conn.close();
});

test('中途断开：在途请求被拒，onLost 触发一次，连接转为不可用', async () => {
  const { sock, conn } = await connected();
  let lost = 0;
  conn.onLost = () => lost++;
  const p = conn.request('GetStreamStatus');
  await sock.waitSent(2);
  sock.onclose(1006, '');
  await assert.rejects(p, /连不上 OBS/);
  assert.equal(conn.alive, false);
  sock.onclose(1006, '');
  assert.equal(lost, 1, 'onLost 只触发一次');
});

test('主动 close 不触发 onLost，之后的请求直接拒', async () => {
  const { sock, conn } = await connected();
  let lost = 0;
  conn.onLost = () => lost++;
  conn.close();
  assert.equal(sock.closed, true);
  assert.equal(lost, 0);
  await assert.rejects(conn.request('GetVersion'), /已关闭/);
});

test('ObsConn 构造即挂上 socket 回调（不依赖 onopen）', () => {
  const sock = new FakeSocket();
  const conn = new ObsConn('ws://localhost:4455', '', () => sock);
  assert.equal(typeof sock.onmessage, 'function');
  assert.equal(typeof sock.onclose, 'function');
  assert.equal(typeof sock.onerror, 'function');
  conn.close();
});
