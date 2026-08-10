import assert from 'node:assert/strict';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import { applyLoosePatch } from './apply-loose-patch.mjs';
import { applyZipTree } from './apply-zip.mjs';
import { normalizePatch } from './normalize-patch.mjs';
import { resolveSafeRepoPath } from './path-policy.mjs';

function withTempRepo(run) {
  const root = mkdtempSync(join(tmpdir(), 'axonhub-openpatch-'));
  const repoRoot = join(root, 'repo');
  mkdirSync(repoRoot);

  try {
    return run({ root, repoRoot });
  } finally {
    rmSync(root, { force: true, recursive: true });
  }
}

test('path policy accepts a normal repository file and rejects traversal or control paths', () => {
  withTempRepo(({ repoRoot }) => {
    assert.equal(resolveSafeRepoPath(repoRoot, 'src/app.js'), join(repoRoot, 'src', 'app.js'));

    for (const candidate of [
      '../outside.txt',
      '/tmp/outside.txt',
      '.git/config',
      '.ai-patches/payload.diff',
      '.github/workflows/pwn.yml',
      '././.github/workflows/pwn.yml',
    ]) {
      assert.throws(() => resolveSafeRepoPath(repoRoot, candidate), /unsafe|protected/i, candidate);
    }
  });
});

test('recursive deletion rejects the repository root and protected-path ancestors', () => {
  withTempRepo(({ repoRoot }) => {
    for (const candidate of ['.', './', '.github', '.github/workflows']) {
      assert.throws(() => resolveSafeRepoPath(repoRoot, candidate, { recursiveDelete: true }), /unsafe|protected/i, candidate);
    }
  });
});

test('path policy rejects a symlink ancestor that escapes the repository', () => {
  withTempRepo(({ root, repoRoot }) => {
    const outside = join(root, 'outside');
    mkdirSync(outside);
    writeFileSync(join(outside, 'victim.txt'), 'outside\n');
    symlinkSync(outside, join(repoRoot, 'linked'));

    assert.throws(() => resolveSafeRepoPath(repoRoot, 'linked/victim.txt'), /symbolic link|outside/i);
  });
});

function patchFor(target, oldLine = 'before', newLine = 'after') {
  return [
    `diff --git a/${target} b/${target}`,
    `--- a/${target}`,
    `+++ b/${target}`,
    '@@ -1,1 +1,1 @@',
    `-${oldLine}`,
    `+${newLine}`,
    '',
  ].join('\n');
}

test('loose patch check-only validates without writing and normal mode applies the hunk', () => {
  withTempRepo(({ repoRoot }) => {
    const target = join(repoRoot, 'src', 'message.txt');
    mkdirSync(join(repoRoot, 'src'));
    writeFileSync(target, 'before\n');
    const patchText = patchFor('src/message.txt');

    applyLoosePatch({ repoRoot, patchText, checkOnly: true });
    assert.equal(readFileSync(target, 'utf8'), 'before\n');

    applyLoosePatch({ repoRoot, patchText, checkOnly: false });
    assert.equal(readFileSync(target, 'utf8'), 'after\n');
  });
});

test('loose patch rejects traversal and repository control files before reading them', () => {
  withTempRepo(({ root, repoRoot }) => {
    const outside = join(root, 'outside.txt');
    writeFileSync(outside, 'before\n');
    mkdirSync(join(repoRoot, '.git'));
    writeFileSync(join(repoRoot, '.git', 'config'), 'before\n');

    for (const target of ['../outside.txt', '.git/config', '.github/workflows/apply.yml']) {
      assert.throws(() => applyLoosePatch({ repoRoot, patchText: patchFor(target), checkOnly: false }), /unsafe|protected/i, target);
    }

    assert.equal(readFileSync(outside, 'utf8'), 'before\n');
    assert.equal(readFileSync(join(repoRoot, '.git', 'config'), 'utf8'), 'before\n');
  });
});

test('loose patch cannot follow a repository symlink to modify an external file', () => {
  withTempRepo(({ root, repoRoot }) => {
    const outside = join(root, 'outside');
    mkdirSync(outside);
    writeFileSync(join(outside, 'victim.txt'), 'before\n');
    symlinkSync(outside, join(repoRoot, 'linked'));

    assert.throws(
      () => applyLoosePatch({ repoRoot, patchText: patchFor('linked/victim.txt'), checkOnly: false }),
      /symbolic link|outside/i
    );
    assert.equal(readFileSync(join(outside, 'victim.txt'), 'utf8'), 'before\n');
  });
});

test('zip tree applies validated copies and deletions', () => {
  withTempRepo(({ root, repoRoot }) => {
    const extractRoot = join(root, 'extract');
    mkdirSync(join(extractRoot, 'src'), { recursive: true });
    writeFileSync(join(extractRoot, 'src', 'new.txt'), 'new\n');
    writeFileSync(join(extractRoot, '.openpatch-delete.txt'), 'old.txt\n');
    writeFileSync(join(repoRoot, 'old.txt'), 'old\n');

    applyZipTree({ repoRoot, extractRoot });

    assert.equal(readFileSync(join(repoRoot, 'src', 'new.txt'), 'utf8'), 'new\n');
    assert.equal(existsSync(join(repoRoot, 'old.txt')), false);
    assert.equal(existsSync(join(repoRoot, '.openpatch-delete.txt')), false);
  });
});

test('zip tree rejects protected recursive deletions before performing any operation', () => {
  for (const deletePath of ['.', '.github', '.github/workflows']) {
    withTempRepo(({ root, repoRoot }) => {
      const extractRoot = join(root, 'extract');
      mkdirSync(extractRoot);
      writeFileSync(join(extractRoot, 'copied.txt'), 'must not be copied\n');
      writeFileSync(join(extractRoot, '.openpatch-delete.txt'), `${deletePath}\n`);
      writeFileSync(join(repoRoot, 'keep.txt'), 'keep\n');

      assert.throws(() => applyZipTree({ repoRoot, extractRoot }), /unsafe|protected/i, deletePath);
      assert.equal(readFileSync(join(repoRoot, 'keep.txt'), 'utf8'), 'keep\n');
      assert.equal(existsSync(join(repoRoot, 'copied.txt')), false);
    });
  }
});

test('zip tree cannot copy through a repository symlink into an external directory', () => {
  withTempRepo(({ root, repoRoot }) => {
    const extractRoot = join(root, 'extract');
    const outside = join(root, 'outside');
    mkdirSync(join(extractRoot, 'linked'), { recursive: true });
    mkdirSync(outside);
    writeFileSync(join(extractRoot, 'linked', 'created.txt'), 'outside write\n');
    symlinkSync(outside, join(repoRoot, 'linked'));

    assert.throws(() => applyZipTree({ repoRoot, extractRoot }), /symbolic link|outside/i);
    assert.equal(existsSync(join(outside, 'created.txt')), false);
  });
});

test('commit title file is treated literally and cannot execute shell syntax', () => {
  withTempRepo(({ root, repoRoot }) => {
    const runGit = (...args) => {
      const result = spawnSync('git', args, { cwd: repoRoot, encoding: 'utf8' });
      assert.equal(result.status, 0, result.stderr);
      return result.stdout.trim();
    };

    runGit('init', '-q');
    runGit('config', 'user.name', 'OpenPatch Test');
    runGit('config', 'user.email', 'openpatch@example.invalid');
    writeFileSync(join(repoRoot, 'tracked.txt'), 'before\n');
    runGit('add', 'tracked.txt');
    runGit('commit', '-qm', 'initial');
    writeFileSync(join(repoRoot, 'tracked.txt'), 'after\n');
    runGit('add', 'tracked.txt');

    const sentinel = join(root, 'title-command-ran');
    const title = `fix: keep $(touch ${sentinel}) literal`;
    const titleFile = join(root, 'commit-title.txt');
    writeFileSync(titleFile, `${title}\n`);
    const script = fileURLToPath(new URL('./commit-patch.sh', import.meta.url));
    const result = spawnSync('bash', [script, titleFile], { cwd: repoRoot, encoding: 'utf8' });

    assert.equal(result.status, 0, result.stderr);
    assert.equal(existsSync(sentinel), false);
    assert.equal(runGit('log', '-1', '--pretty=%s'), title);
  });
});

test('patch normalization preserves a safe target and rejects protected workflow targets', () => {
  withTempRepo(({ repoRoot }) => {
    const safe = patchFor('src/message.txt');
    assert.equal(normalizePatch(safe, repoRoot), safe);

    assert.throws(() => normalizePatch(patchFor('.github/workflows/pwn.yml'), repoRoot), /protected/i);
  });
});
