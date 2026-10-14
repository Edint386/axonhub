import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const source = readFileSync(join(import.meta.dirname, 'channels-rate-limit-dialog.tsx'), 'utf8');

test('quota window times are formatted in the channel quota timezone', () => {
  const start = source.indexOf('const formatQuotaWindowTime');
  const end = source.indexOf('return format(date', start);
  assert.ok(start !== -1 && end > start, 'formatQuotaWindowTime should exist');

  // Ignore comments so explaining the rule does not trip the check below.
  const block = source.slice(start, end).replace(/\/\/.*$/gm, '');
  assert.match(block, /timeZone:\s*savedQuotaTimezone/);
  assert.match(block, /timeZoneName:\s*'short'/);
  // Intl throws when timeZoneName is combined with dateStyle/timeStyle.
  assert.doesNotMatch(block, /dateStyle|timeStyle/);
});

test('the Intl options used for quota window times are accepted', () => {
  const options = {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
    second: '2-digit',
    timeZone: 'Asia/Tokyo',
    timeZoneName: 'short',
  };
  const text = new Intl.DateTimeFormat('en', options).format(new Date('2026-10-14T15:00:00Z'));
  assert.match(text, /Oct 15, 2026/);
  assert.match(text, /12:00:00\s?AM/);
  assert.match(text, /GMT\+9/);
});
