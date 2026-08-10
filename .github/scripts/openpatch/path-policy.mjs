import { existsSync, lstatSync, realpathSync } from 'node:fs';
import { isAbsolute, join, relative, resolve, sep } from 'node:path';

const PROTECTED_PATHS = ['.git', '.ai-patches', '.github/workflows'];

function isSameOrDescendant(candidate, protectedPath) {
  return candidate === protectedPath || candidate.startsWith(`${protectedPath}/`);
}

function isInside(root, target) {
  const value = relative(root, target);
  return value === '' || (!value.startsWith(`..${sep}`) && value !== '..' && !isAbsolute(value));
}

export function normalizeRepoRelativePath(candidate) {
  const value = String(candidate ?? '')
    .replaceAll('\\', '/')
    .replace(/^\.\//, '')
    .trim();
  const parts = value.split('/').filter(Boolean);

  if (
    !value ||
    value === '.' ||
    isAbsolute(value) ||
    /^[A-Za-z]:\//.test(value) ||
    parts.includes('.') ||
    parts.includes('..') ||
    value.includes('\0')
  ) {
    throw new Error(`Unsafe repository path: ${candidate}`);
  }

  const normalized = parts.join('/');
  if (PROTECTED_PATHS.some((protectedPath) => isSameOrDescendant(normalized, protectedPath))) {
    throw new Error(`Protected repository path: ${candidate}`);
  }

  return normalized;
}

export function resolveSafeRepoPath(repoRoot, candidate, { recursiveDelete = false } = {}) {
  const normalized = normalizeRepoRelativePath(candidate);

  if (
    recursiveDelete &&
    PROTECTED_PATHS.some(
      (protectedPath) => isSameOrDescendant(normalized, protectedPath) || isSameOrDescendant(protectedPath, normalized)
    )
  ) {
    throw new Error(`Protected recursive deletion path: ${candidate}`);
  }

  const absoluteRoot = resolve(repoRoot);
  const canonicalRoot = realpathSync(absoluteRoot);
  const destination = resolve(absoluteRoot, normalized);

  if (!isInside(absoluteRoot, destination)) {
    throw new Error(`Unsafe repository path outside root: ${candidate}`);
  }

  let current = canonicalRoot;
  for (const part of normalized.split('/')) {
    current = join(current, part);
    if (!existsSync(current)) {
      break;
    }

    if (lstatSync(current).isSymbolicLink()) {
      throw new Error(`Symbolic link paths are not allowed: ${candidate}`);
    }

    const canonicalCurrent = realpathSync(current);
    if (!isInside(canonicalRoot, canonicalCurrent)) {
      throw new Error(`Repository path resolves outside root: ${candidate}`);
    }
    current = canonicalCurrent;
  }

  return destination;
}
