#!/usr/bin/env node

import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { resolveSafeRepoPath } from './path-policy.mjs';

function cleanPath(input) {
  const value = String(input ?? '').trim().replace(/^([ab])\//, '');
  if (value === '/dev/null') return value;
  return value
    .replace(/\.title_before$/i, '')
    .replace(/_before$/i, '')
    .replace(/\.before$/i, '')
    .replace(/\.orig$/i, '')
    .replace(/\.old$/i, '');
}

function assertTarget(repoRoot, target) {
  if (target !== '/dev/null') {
    resolveSafeRepoPath(repoRoot, target);
  }
}

export function normalizePatch(input, repoRoot) {
  const lines = input.split(/\r?\n/);
  const out = [];
  let currentTarget = '';

  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i];
    const match = line.match(/^diff --git\s+(.+?)\s+(.+)$/);
    if (match) {
      const oldPath = cleanPath(match[1]);
      const newPath = cleanPath(match[2]);
      currentTarget = newPath !== '/dev/null' ? newPath : oldPath;
      assertTarget(repoRoot, currentTarget);
      out.push(`diff --git a/${currentTarget} b/${currentTarget}`);

      let next = i + 1;
      while (next < lines.length && lines[next] === '') {
        next += 1;
      }
      if (lines[next]?.startsWith('@@')) {
        out.push(`--- a/${currentTarget}`);
        out.push(`+++ b/${currentTarget}`);
      }
      continue;
    }

    if (line.startsWith('--- ')) {
      const oldPath = cleanPath(line.replace(/^---\s+/, '').split('\t')[0]);
      const nextLine = lines[i + 1] ?? '';
      const plusMatch = nextLine.match(/^\+\+\+\s+(.+)$/);

      if (!currentTarget && plusMatch) {
        const newPath = cleanPath(plusMatch[1].split('\t')[0]);
        currentTarget = newPath !== '/dev/null' ? newPath : oldPath;
        assertTarget(repoRoot, currentTarget);
        out.push(`diff --git a/${currentTarget} b/${currentTarget}`);
      }

      if (currentTarget) {
        out.push(`--- a/${currentTarget}`);
        continue;
      }
    }

    if (line.startsWith('+++ ') && currentTarget) {
      out.push(`+++ b/${currentTarget}`);
      continue;
    }

    out.push(line);
  }

  return out.join('\n');
}

const isMain = process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1];
if (isMain) {
  const [inputPath, outputPath] = process.argv.slice(2);
  if (!inputPath || !outputPath) {
    throw new Error('Usage: normalize-patch.mjs INPUT_PATCH OUTPUT_PATCH');
  }
  writeFileSync(outputPath, normalizePatch(readFileSync(inputPath, 'utf8'), process.cwd()));
}
