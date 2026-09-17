import assert from 'node:assert/strict';
import test from 'node:test';
import { addScreenBitrateFloor } from './tmp/sdp-floor.js';

const sdp = (lines) => lines.join('\r\n');

test('投屏段（起始码率 > 1000）补上下限，取起始的 55%', () => {
  const out = addScreenBitrateFloor(
    sdp(['v=0', 'm=video 9 UDP/TLS/RTP/SAVPF 96', 'a=fmtp:96 level-asymmetry-allowed=1;x-google-start-bitrate=7200']),
  );
  assert.match(out, /x-google-min-bitrate=3960/);
});

test('摄像头段（起始码率被 LiveKit 封在 1000）不动', () => {
  const line = 'a=fmtp:96 x-google-start-bitrate=1000';
  assert.equal(addScreenBitrateFloor(sdp(['v=0', line])), sdp(['v=0', line]));
});

test('没有起始码率、已有下限、非 fmtp 行一律原样', () => {
  for (const line of ['a=fmtp:96 profile-level-id=42e01f', 'a=fmtp:96 x-google-start-bitrate=5000;x-google-min-bitrate=1', 'a=rtpmap:96 H264/90000']) {
    assert.equal(addScreenBitrateFloor(sdp(['v=0', line])), sdp(['v=0', line]));
  }
});

test('一份描述里多条投屏 fmtp 都补上', () => {
  const out = addScreenBitrateFloor(
    sdp(['a=fmtp:96 x-google-start-bitrate=7200', 'a=fmtp:98 x-google-start-bitrate=4000']),
  );
  assert.equal((out.match(/x-google-min-bitrate=/g) ?? []).length, 2);
});
