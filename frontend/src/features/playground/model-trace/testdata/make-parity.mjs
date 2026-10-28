// Builds parity fixtures: runs ModelTrace's original browser core on reference outputs.
import { readFileSync, writeFileSync } from 'node:fs';
// usage: node make-parity.mjs <fingerprint-core.mjs> <unified_bank.json> <claude_reference.jsonl> <gpt_reference.jsonl> <out.json> [commit]
const [oraclePath, bankPath, claudeRef, gptRef, outPath, commit = 'unknown'] = process.argv.slice(2);
const { analyzeGlobalOutputs } = await import(oraclePath);
const bank = JSON.parse(readFileSync(bankPath, 'utf8'));
const rows = [claudeRef, gptRef].flatMap((p) => readFileSync(p, 'utf8').trim().split('\n').map((l) => JSON.parse(l)))
  .filter((r) => r.strict_valid && r.text);
const pick = (model, n, offset = 0) => {
  const list = rows.filter((r) => r.model_id === model);
  const step = Math.max(1, Math.floor(list.length / (n + 1)));
  return Array.from({ length: n }, (_, i) => list[(offset + (i + 1) * step) % list.length]);
};
const asOutput = (r) => ({ text: r.text, expected_count: r.requested_count });
const cases = [];
for (const model of ['claude-opus-4-7', 'claude-sonnet-5-5', 'claude-haiku-4-5-20251001', 'gpt-5.5', 'gpt-6-sol', 'gpt-5.6-luna']) {
  cases.push({ name: `three outputs from ${model}`, outputs: pick(model, 3).map(asOutput) });
}
cases.push({ name: 'one output from gpt-5.4', outputs: pick('gpt-5.4', 1).map(asOutput) });
cases.push({ name: 'two outputs from claude-opus-5', outputs: pick('claude-opus-5', 2).map(asOutput) });
const mixed = pick('claude-opus-4-8', 3).map(asOutput);
mixed[1] = { text: mixed[1].text.split(/[,\s]+/).slice(0, 40).join(', '), expected_count: mixed[1].expected_count };
cases.push({ name: 'one truncated output is ignored', outputs: mixed });
let seed = 12345;
const rand = () => ((seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648);
const uniform = Array.from({ length: 3 }, (_, i) => {
  const n = 300 + i * 7;
  return { text: Array.from({ length: n }, () => 1 + Math.floor(rand() * 355)).join(', '), expected_count: n };
});
cases.push({ name: 'uniform synthetic numbers', outputs: uniform });
for (const c of cases) c.expected = analyzeGlobalOutputs(c.outputs, bank);
writeFileSync(outPath, JSON.stringify({ source: `ModelTrace static/fingerprint-core.js @ ${commit}`, cases }) + '\n');
for (const c of cases) console.log(c.name.padEnd(45), c.expected.prediction, c.expected.probability.toFixed(4), c.expected.used_outputs);
