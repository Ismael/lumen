@echo off
setlocal enabledelayedexpansion

:: Verifies the production dispatch path on Windows: an extensionless
:: command path (the one stored in plugin manifests) is resolved by
:: cmd.exe's PATHEXT search to scripts\run.cmd, which then runs without
:: echoing any of its own lines to stdout. This is the byte-equality
:: test that guards MCP stdio JSON-RPC framing against the polyglot
:: regressions that bit us in PR #145 and PR #157.

set "PASS=0"
set "FAIL=0"

set "TMP_DIR=%TEMP%\lumen-test-%RANDOM%"
mkdir "%TMP_DIR%\scripts" 2>NUL

:: Mock launcher: a run.cmd that prints exactly one line. If the
:: production launcher echoed any of its own lines (the polyglot bug),
:: the test's strict line-count assertion below would catch it.
(
  echo @echo off
  echo echo {"mock":"ok"}
) > "%TMP_DIR%\scripts\run.cmd"

:: --- Test 1: cmd /c resolves extensionless path via PATHEXT ---
:: Mirrors how Claude Code (shell:true hooks) and cross-spawn (MCP
:: transport) end up invoking the launcher on Windows.
set "STDOUT_FILE=%TMP_DIR%\stdout.txt"
set "STDERR_FILE=%TMP_DIR%\stderr.txt"
cmd /c "%TMP_DIR%\scripts\run" stdio >"%STDOUT_FILE%" 2>"%STDERR_FILE%"
set "EXIT_CODE=%ERRORLEVEL%"

if "%EXIT_CODE%"=="0" (
  echo   PASS: cmd /c found run.cmd via PATHEXT for extensionless path
  set /a PASS+=1
) else (
  echo   FAIL: cmd /c could not resolve extensionless path ^(exit %EXIT_CODE%^)
  echo         stdout:
  type "%STDOUT_FILE%"
  echo         stderr:
  type "%STDERR_FILE%"
  set /a FAIL+=1
)

:: --- Test 2: stdout is byte-exact "{"mock":"ok"}" with NO leading echo ---
:: The polyglot regressions (PR #145, PR #157) all surfaced as extra
:: bytes on stdout: a leading prompt line, a "not recognized" error,
:: or the unwrapped first line of the polyglot itself. MCP JSON-RPC
:: framing breaks on the first stray byte. We assert exact bytes.
set "EXPECTED={"mock":"ok"}"
set "GOT="
for /f "usebackq delims=" %%i in ("%STDOUT_FILE%") do (
  if not defined GOT (
    set "GOT=%%i"
  ) else (
    set "GOT=!GOT!|%%i"
  )
)

if "!GOT!"=="!EXPECTED!" (
  echo   PASS: stdout is byte-exact, zero echoed lines from launcher
  set /a PASS+=1
) else (
  echo   FAIL: stdout mismatch ^(launcher polluted output^)
  echo         expected: !EXPECTED!
  echo         got:      !GOT!
  set /a FAIL+=1
)

:: --- Test 3: stderr empty ---
:: A fully clean dispatch produces zero stderr. Any "not recognized",
:: "Bad command", "/bin/sh: ... No such file" output here means the
:: launcher leaked or the dispatch path is wrong.
set "STDERR_SIZE=0"
for %%A in ("%STDERR_FILE%") do set "STDERR_SIZE=%%~zA"

if "%STDERR_SIZE%"=="0" (
  echo   PASS: stderr is empty
  set /a PASS+=1
) else (
  echo   FAIL: stderr not empty ^(%STDERR_SIZE% bytes^)
  type "%STDERR_FILE%"
  set /a FAIL+=1
)

:: --- Test 4: real scripts\run.cmd ^(extensionless^) does not echo to stdout ---
:: Use the actual repo file, not the mock, and assert the FIRST byte
:: of stdout is NOT '@' (which would indicate the launcher echoed
:: line 1 of itself before running). This guards against future
:: regressions where someone removes "@echo off" and reintroduces
:: polyglot-style echoing.
set "REAL_STDOUT=%TMP_DIR%\real_stdout.txt"
set "REAL_STDERR=%TMP_DIR%\real_stderr.txt"

:: Invoke a known-safe subcommand so the dispatch exits cleanly and we
:: can assert both (a) exit 0 and (b) that the FIRST line of stdout is
:: run.cmd's intentional output, never an echoed source line. Note
:: lumen uses `version` as a subcommand — `--version` is not a flag
:: and would return exit 1 with cobra's usage help on stderr.
cmd /c "%~dp0run" version >"%REAL_STDOUT%" 2>"%REAL_STDERR%"
set "REAL_EXIT=%ERRORLEVEL%"

set "FIRST_CHAR="
for /f "usebackq delims=" %%i in ("%REAL_STDOUT%") do (
  if not defined FIRST_CHAR set "FIRST_CHAR=%%i"
)

set "STARTS_WITH_AT=0"
if defined FIRST_CHAR (
  if "!FIRST_CHAR:~0,1!"=="@" set "STARTS_WITH_AT=1"
)

if not "!REAL_EXIT!"=="0" (
  echo   FAIL: real run dispatch failed ^(exit !REAL_EXIT!^)
  echo         stdout:
  type "%REAL_STDOUT%"
  echo         stderr:
  type "%REAL_STDERR%"
  set /a FAIL+=1
) else if "!STARTS_WITH_AT!"=="1" (
  echo   FAIL: real run.cmd echoed '@echo off' to stdout
  echo         first stdout char was '@', meaning ECHO is on
  set /a FAIL+=1
) else (
  echo   PASS: real run.cmd does not echo source lines to stdout
  set /a PASS+=1
)

:: Cleanup
rmdir /s /q "%TMP_DIR%" 2>NUL

:: Summary
echo.
echo === summary ===
echo   passed: %PASS%
echo   failed: %FAIL%

if %FAIL% GTR 0 exit /b 1
exit /b 0
