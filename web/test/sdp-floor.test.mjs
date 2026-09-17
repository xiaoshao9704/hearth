import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { addScreenBitrateFloor, setScreenBitrateFloorKbps } from './tmp/sdp-floor.js';

const sdp = (lines) => lines.join('\r\n');
const screenSdp = sdp(['v=0', 'm=video 9 UDP/TLS/RTP/SAVPF 96', 'a=fmtp:96 level-asymmetry-allowed=1;x-google-start-bitrate=7200']);

test('没设下限时一个字节都不改', () => {
  setScreenBitrateFloorKbps(0);
  assert.equal(addScreenBitrateFloor(screenSdp), screenSdp);
  setScreenBitrateFloorKbps(-5);
  assert.equal(addScreenBitrateFloor(screenSdp), screenSdp);
});

test('投屏段（起始码率 > 1000）注入设定的绝对值', () => {
  setScreenBitrateFloorKbps(3500);
  assert.match(addScreenBitrateFloor(screenSdp), /x-google-min-bitrate=3500/);
  setScreenBitrateFloorKbps(1200);
  assert.match(addScreenBitrateFloor(screenSdp), /x-google-min-bitrate=1200/);
});

test('摄像头段（起始码率被 LiveKit 封在 1000）不动', () => {
  setScreenBitrateFloorKbps(3500);
  const line = 'a=fmtp:96 x-google-start-bitrate=1000';
  assert.equal(addScreenBitrateFloor(sdp(['v=0', line])), sdp(['v=0', line]));
});

test('没有起始码率、已有下限、非 fmtp 行一律原样', () => {
  setScreenBitrateFloorKbps(3500);
  for (const line of ['a=fmtp:96 profile-level-id=42e01f', 'a=fmtp:96 x-google-start-bitrate=5000;x-google-min-bitrate=1', 'a=rtpmap:96 H264/90000']) {
    assert.equal(addScreenBitrateFloor(sdp(['v=0', line])), sdp(['v=0', line]));
  }
});

test('一份描述里多条投屏 fmtp 都补上', () => {
  setScreenBitrateFloorKbps(2000);
  const out = addScreenBitrateFloor(
    sdp(['a=fmtp:96 x-google-start-bitrate=7200', 'a=fmtp:98 x-google-start-bitrate=4000']),
  );
  assert.equal((out.match(/x-google-min-bitrate=2000/g) ?? []).length, 2);
});

// 码率范围只在发布那一刻定死：热应用路径要是又去改上限，就会出现「上限立刻生效、下限要等
// 下次投屏」的不一致。这条用源码守住——热应用跑在真 PeerConnection 上，没法在这里跑起来。
test('投屏画质热应用路径不碰码率', () => {
  const src = readFileSync(new URL('../src/engine/livekit.ts', import.meta.url), 'utf8');
  const body = src.slice(src.indexOf('async applyScreenPrefs()'), src.indexOf('private screenOptions('));
  assert.ok(body.length > 0, '没找到 applyScreenPrefs');
  assert.doesNotMatch(body, /maxBitrate|bitrateMax|bitrateMin/);
  const republish = src.slice(src.indexOf('async republishScreen()'), src.indexOf('async applyScreenPrefs()'));
  assert.match(republish, /setScreenBitrateFloorKbps\(/);
});
