#!/usr/bin/env node

import { copyFileSync, lstatSync, mkdirSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { dirname, join, relative, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

import { resolveSafeRepoPath } from './path-policy.mjs';

const DELETE_LIST = '.openpatch-delete.txt';

function walkFiles(root, current = root) {
  const files = [];
  for (const entry of readdirSync(current, { withFileTypes: true })) {
    const fullPath = join(current, entry.name);
    const stat = lstatSync(fullPath);
    if (stat.isSymbolicLink()) {
      throw new Error(`Symbolic links are not allowed in zip packages: ${relative(root, fullPath)}`);
    }
    if (entry.isDirectory()) {
      files.push(...walkFiles(root, fullPath));
    } else if (entry.isFile()) {
      files.push(fullPath);
    }
  }
  return files;
}

function toRepoRelative(root, file) {
  return relative(root, file).split(sep).join('/');
}

export function applyZipTree({ repoRoot, extractRoot }) {
  const files = walkFiles(extractRoot);
  const deletePaths = [];
  const copies = [];

  for (const sourcePath of files) {
    const relativePath = toRepoRelative(extractRoot, sourcePath);
    if (relativePath.startsWith('__MACOSX/') || relativePath.endsWith('.DS_Store')) {
      continue;
    }

    if (relativePath === DELETE_LIST) {
      for (const line of readFileSync(sourcePath, 'utf8').split(/\r?\n/)) {
        const candidate = line.trim();
        if (candidate && !candidate.startsWith('#')) {
          deletePaths.push(candidate);
        }
      }
      continue;
    }

    copies.push({ sourcePath, relativePath });
  }

  const validatedDeletes = deletePaths.map((candidate) => resolveSafeRepoPath(repoRoot, candidate, { recursiveDelete: true }));
  const validatedCopies = copies.map(({ sourcePath, relativePath }) => ({
    sourcePath,
    destination: resolveSafeRepoPath(repoRoot, relativePath),
  }));

  for (const targetPath of validatedDeletes) {
    rmSync(targetPath, { force: true, recursive: true });
  }
  for (const { sourcePath, destination } of validatedCopies) {
    mkdirSync(dirname(destination), { recursive: true });
    copyFileSync(sourcePath, destination);
  }
}

const isMain = process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1];
if (isMain) {
  const extractRoot = process.argv[2];
  if (!extractRoot) {
    throw new Error('Usage: apply-zip.mjs EXTRACTED_DIRECTORY');
  }
  applyZipTree({ repoRoot: process.cwd(), extractRoot });
}
