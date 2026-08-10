#!/usr/bin/env node

import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { resolveSafeRepoPath } from './path-policy.mjs';

function splitText(text) {
  return text.endsWith('\n') ? text.slice(0, -1).split('\n') : text.split('\n');
}

function countMatches(lines, needle) {
  let count = 0;
  let index = -1;
  if (needle.length === 0) return { count, index };

  for (let i = 0; i <= lines.length - needle.length; i += 1) {
    let matches = true;
    for (let j = 0; j < needle.length; j += 1) {
      if (lines[i + j] !== needle[j]) {
        matches = false;
        break;
      }
    }
    if (matches) {
      count += 1;
      index = i;
    }
  }

  return { count, index };
}

function parseLoosePatch(patchText) {
  const patch = patchText.split(/\r?\n/);
  const files = [];
  let file = null;
  let hunkCount = 0;

  for (let i = 0; i < patch.length; i += 1) {
    const diff = patch[i].match(/^diff --git\s+a\/(.+?)\s+b\/(.+)$/);
    if (diff) {
      file = { path: diff[2], hunks: [] };
      files.push(file);
      continue;
    }

    const hunk = patch[i].match(/^@@\s+-(\d+)(?:,(\d+))?\s+\+(\d+)(?:,(\d+))?\s+@@/);
    if (hunk && file) {
      const lines = [];
      i += 1;
      while (i < patch.length && !patch[i].startsWith('diff --git ') && !patch[i].startsWith('@@ ')) {
        lines.push(patch[i]);
        i += 1;
      }
      i -= 1;
      file.hunks.push({ oldStart: Number(hunk[1]), lines });
      hunkCount += 1;
    }
  }

  if (files.length === 0 || hunkCount === 0) {
    throw new Error('No patch hunks were parsed.');
  }

  return files;
}

export function applyLoosePatch({ repoRoot, patchText, checkOnly = false }) {
  const files = parseLoosePatch(patchText);
  const pendingWrites = [];

  for (const filePatch of files) {
    if (filePatch.hunks.length === 0) {
      throw new Error(`No hunks found for ${filePatch.path}.`);
    }

    const targetPath = resolveSafeRepoPath(repoRoot, filePatch.path);
    if (!existsSync(targetPath)) {
      throw new Error(`File not found: ${filePatch.path}`);
    }

    let lines = splitText(readFileSync(targetPath, 'utf8'));
    for (const hunk of filePatch.hunks) {
      const oldParts = hunk.lines
        .filter((line) => line.startsWith(' ') || line.startsWith('-'))
        .map((line) => line.slice(1));
      const newParts = hunk.lines
        .filter((line) => line.startsWith(' ') || line.startsWith('+'))
        .map((line) => line.slice(1));
      const removeParts = hunk.lines.filter((line) => line.startsWith('-')).map((line) => line.slice(1));
      const addParts = hunk.lines.filter((line) => line.startsWith('+')).map((line) => line.slice(1));

      let found = countMatches(lines, oldParts);
      if (found.count === 1) {
        lines.splice(found.index, oldParts.length, ...newParts);
        continue;
      }

      found = countMatches(lines, removeParts);
      if (found.count === 1) {
        lines.splice(found.index, removeParts.length, ...addParts);
        continue;
      }

      throw new Error(`Cannot safely apply hunk in ${filePatch.path} near original line ${hunk.oldStart}`);
    }

    pendingWrites.push({ targetPath, content: `${lines.join('\n')}\n` });
  }

  if (!checkOnly) {
    for (const pending of pendingWrites) {
      writeFileSync(pending.targetPath, pending.content);
    }
  }
}

const isMain = process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1];
if (isMain) {
  const args = process.argv.slice(2);
  const checkOnly = args.includes('--check');
  const patchPath = args.find((arg) => arg !== '--check');
  if (!patchPath) {
    throw new Error('Usage: apply-loose-patch.mjs [--check] PATCH_FILE');
  }

  applyLoosePatch({
    repoRoot: process.cwd(),
    patchText: readFileSync(patchPath, 'utf8'),
    checkOnly,
  });
}
