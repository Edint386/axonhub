import { decompress } from 'fzstd';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { decodeDetectorSnapshot } from './detector.ts';
import { sha256Bytes, sha256Hex } from './sha256.ts';

const dir = import.meta.dirname;
const bundle = JSON.parse(readFileSync(join(dir, 'data', 'bundle.json'), 'utf8'));
const compressed = readFileSync(join(dir, 'data', 'shared_detector.bin.zst'));

function nodeSha256Hex(bytes) {
  return createHash('sha256').update(bytes).digest('hex');
}

function withoutSubtle(run) {
  Object.defineProperty(crypto, 'subtle', { value: undefined, configurable: true });
  try {
    assert.equal(crypto.subtle, undefined);
    return run();
  } finally {
    delete crypto.subtle;
  }
}

test('sha256Hex matches Web Crypto and Node for known vectors', async () => {
  const vectors = [
    new Uint8Array(),
    new TextEncoder().encode('abc'),
    new TextEncoder().encode('hello'),
    new Uint8Array(64).fill(0x5a),
    new Uint8Array(65).fill(0x5a),
    new Uint8Array(1000).map((_, index) => index & 0xff),
  ];
  for (const bytes of vectors) {
    const expected = nodeSha256Hex(bytes);
    const native = await crypto.subtle.digest('SHA-256', bytes);
    assert.equal(await sha256Hex(bytes), expected);
    assert.equal(Buffer.from(native).toString('hex'), expected);
    assert.equal(Buffer.from(sha256Bytes(bytes)).toString('hex'), expected);
  }
  const abc = await sha256Hex(new TextEncoder().encode('abc'));
  assert.equal(abc, 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
});

test('sha256Hex still works when crypto.subtle is missing', async () => {
  const bytes = new TextEncoder().encode('abc');
  const expected = 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad';
  const hex = await withoutSubtle(() => sha256Hex(bytes));
  assert.equal(hex, expected);
  assert.ok(crypto.subtle);
});

test('loads the official snapshot when crypto.subtle is missing', async () => {
  const loaded = await withoutSubtle(() => decodeDetectorSnapshot(bundle, compressed));
  assert.equal(loaded.bank.models.length, 75);
  assert.equal(loaded.bundle.files['shared_detector.bin'].sha256, bundle.files['shared_detector.bin'].sha256);
  const uncompressed = decompress(compressed);
  assert.equal(nodeSha256Hex(uncompressed), bundle.files['shared_detector.bin'].sha256);
});

test('still rejects a checksum mismatch when crypto.subtle is missing', async () => {
  const bad = { ...bundle, files: { 'shared_detector.bin': { ...bundle.files['shared_detector.bin'], sha256: '00'.repeat(32) } } };
  await assert.rejects(
    withoutSubtle(() => decodeDetectorSnapshot(bad, compressed)),
    /does not match its checksum/
  );
});
