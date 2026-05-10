#!/bin/sh
# 2>NUL & @goto batch
exec "$(dirname "$0")/run.sh" "$@"

:batch
@echo off
set "SCRIPT_DIR=%~dp0"
call "%SCRIPT_DIR%run.bat" %*
exit /b %ERRORLEVEL%
