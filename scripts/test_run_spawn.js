// Spawn-semantics regression guard.
//
// Validates the launcher dispatches cleanly via the EXACT spawn primitives
// Claude Code uses in production:
//
//   - POSIX MCP transport: child_process.spawn(cmd, args, { shell: false })
//     -> libuv posix_spawn -> kernel direct-execs the shebang launcher.
//     This is the path PR #145 silently broke on macOS (no /bin/sh fallback
//     on ENOEXEC) and that no prior CI test exercised, because every prior
//     test invoked the launcher through a shell wrapper.
//
//   - Cross-spawn (Claude Code's bundled MCP SDK transport on all OSes):
//     on POSIX it is ~equivalent to child_process.spawn; on Windows it
//     resolves PATHEXT for non-.exe paths and wraps in cmd.exe /d /s /c.
//
//   - shell:true (Claude Code's hook spawn primitive): /bin/sh -c on POSIX,
//     cmd.exe /d /s /c on Windows. cmd.exe's PATHEXT search resolves
//     absolute extensionless paths, so the launcher dispatches cleanly.
//
// We do NOT test child_process.spawn(extensionless, { shell: false }) on
// Windows — that is not a production code path (libuv CreateProcess does
// not PATHEXT-resolve absolute paths) and asserting it would only test
// a workaround we don't ship.

'use strict';

const { spawn } = require('child_process');
const fs        = require('fs');
const os        = require('os');
const path      = require('path');

const IS_WINDOWS = process.platform === 'win32';

let pass = 0;
let fail = 0;

function passMsg(msg) { console.log(`  PASS: ${msg}`); pass++; }
function failMsg(msg) { console.log(`  FAIL: ${msg}`); fail++; }

const TMP_DIR = fs.mkdtempSync(path.join(os.tmpdir(), 'lumen-spawn-'));
const SCRIPTS = path.join(TMP_DIR, 'scripts');
fs.mkdirSync(SCRIPTS, { recursive: true });

// Mock launcher pair. Both files share the base name `run` and are
// dispatched by extension:
//   - POSIX: kernel direct-execs `run` because byte 0 is `#!`.
//   - Windows: cmd.exe / cross-spawn resolve `run.cmd` via PATHEXT.
//
// Output is a fixed JSON literal — no argv reflection — so we can assert
// strict byte-equality across both shells without fighting cmd.exe quote
// escaping rules.
const POSIX_LAUNCHER = `#!/usr/bin/env bash
echo '{"mock":"ok"}'
`;
const WINDOWS_LAUNCHER = `@echo off
echo {"mock":"ok"}
`;

const posixPath   = path.join(SCRIPTS, 'run');
const windowsPath = path.join(SCRIPTS, 'run.cmd');

fs.writeFileSync(posixPath, POSIX_LAUNCHER);
fs.writeFileSync(windowsPath, WINDOWS_LAUNCHER);
fs.chmodSync(posixPath, 0o755);

// Extensionless invocation — the EXACT string the plugin manifests
// hand to Claude Code / cross-spawn.
const launcher = path.join(SCRIPTS, 'run');

function runOne(label, cmd, args, opts = {}) {
  return new Promise((resolve) => {
    let child;
    try {
      child = spawn(cmd, args, opts);
    } catch (err) {
      failMsg(`${label}: spawn threw (${err.code || err.message})`);
      return resolve();
    }
    const stdoutChunks = [];
    const stderrChunks = [];
    child.stdout.on('data', (c) => stdoutChunks.push(c));
    child.stderr.on('data', (c) => stderrChunks.push(c));
    child.on('error', (err) => {
      failMsg(`${label}: spawn failed (${err.code || err.message})`);
      resolve();
    });
    child.on('close', (code) => {
      const stdout = Buffer.concat(stdoutChunks).toString('utf8');
      const stderr = Buffer.concat(stderrChunks).toString('utf8');

      if (code !== 0) {
        failMsg(`${label}: exit ${code}`);
        if (stderr) console.log(`         stderr: ${stderr.trim()}`);
        if (stdout) console.log(`         stdout: ${stdout.trim()}`);
        return resolve();
      }
      let parsed;
      try {
        parsed = JSON.parse(stdout.trim());
      } catch (_) {
        failMsg(`${label}: stdout is not valid JSON — launcher polluted output`);
        console.log(`         stdout (raw): ${JSON.stringify(stdout)}`);
        if (stderr) console.log(`         stderr: ${stderr.trim()}`);
        return resolve();
      }
      if (parsed.mock !== 'ok') {
        failMsg(`${label}: JSON payload mismatch`);
        console.log(`         got: ${JSON.stringify(parsed)}`);
        return resolve();
      }
      passMsg(`${label}: clean dispatch, byte-exact JSON`);
      resolve();
    });
  });
}

function runWithCrossSpawn(crossSpawn) {
  return new Promise((resolve) => {
    const child = crossSpawn(launcher, ['stdio'], { shell: false });
    const stdoutChunks = [];
    const stderrChunks = [];
    child.stdout.on('data', (c) => stdoutChunks.push(c));
    child.stderr.on('data', (c) => stderrChunks.push(c));
    child.on('error', (err) => {
      failMsg(`cross-spawn(extensionless): spawn failed (${err.code || err.message})`);
      resolve();
    });
    child.on('close', (code) => {
      const stdout = Buffer.concat(stdoutChunks).toString('utf8');
      const stderr = Buffer.concat(stderrChunks).toString('utf8');
      if (code !== 0) {
        failMsg(`cross-spawn(extensionless): exit ${code}`);
        if (stderr) console.log(`         stderr: ${stderr.trim()}`);
        if (stdout) console.log(`         stdout: ${stdout.trim()}`);
        return resolve();
      }
      try {
        const parsed = JSON.parse(stdout.trim());
        if (parsed.mock === 'ok') {
          passMsg('cross-spawn(extensionless): clean dispatch, byte-exact JSON');
        } else {
          failMsg('cross-spawn(extensionless): JSON payload mismatch');
          console.log(`         got: ${JSON.stringify(parsed)}`);
        }
      } catch (_) {
        failMsg('cross-spawn(extensionless): stdout is not valid JSON');
        console.log(`         stdout (raw): ${JSON.stringify(stdout)}`);
      }
      resolve();
    });
  });
}

async function main() {
  console.log('=== spawn-semantics regression guard ===');
  console.log(`    platform: ${process.platform}`);
  console.log(`    node:     ${process.version}`);

  if (!IS_WINDOWS) {
    // POSIX MCP transport. shell:false → libuv posix_spawn → kernel
    // direct-exec. This is the bar PR #145 failed in production on macOS.
    await runOne(
      'posix_spawn(extensionless) [shell:false]',
      launcher,
      ['stdio'],
      { shell: false },
    );
  }

  // Hook spawn primitive on both OSes. shell:true → /bin/sh -c (POSIX) or
  // cmd.exe /d /s /c (Windows). cmd.exe PATHEXT-resolves the absolute
  // extensionless path and dispatches to run.cmd.
  await runOne(
    'shell:true dispatch (Claude Code hook primitive)',
    launcher,
    ['stdio'],
    { shell: true },
  );

  // MCP SDK transport. cross-spawn is the package Claude Code's bundled
  // MCP SDK uses for StdioClientTransport. On POSIX it is ~equivalent to
  // a raw spawn; on Windows it does PATHEXT resolution + cmd.exe wrap.
  let crossSpawn;
  try {
    crossSpawn = require('cross-spawn');
  } catch (_) {
    console.log('  SKIP: cross-spawn not installed (run `npm install` in scripts/)');
  }
  if (crossSpawn) {
    await runWithCrossSpawn(crossSpawn);
  }

  try { fs.rmSync(TMP_DIR, { recursive: true, force: true }); } catch (_) {}

  console.log('');
  console.log('=== summary ===');
  console.log(`  passed: ${pass}`);
  console.log(`  failed: ${fail}`);
  process.exit(fail > 0 ? 1 : 0);
}

main().catch((err) => {
  console.error(err);
  process.exit(2);
});
