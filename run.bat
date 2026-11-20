@echo off
rem Builds prbot.exe from source and starts it.
rem Any arguments are passed to the bot, for example:
rem   run.bat -listen 0.0.0.0:8080
rem   run.bat -data data-dev -listen 127.0.0.1:8087
setlocal

rem Run from the repository folder so the default "data" folder is the one here.
rem pushd, unlike cd, also works when the repository is on a network share
rem (\\server\share), by mapping it to a temporary drive letter.
pushd "%~dp0" || (
    echo Could not change to the folder that holds run.bat: %~dp0
    goto :fail
)
set "PUSHED=1"

where go >nul 2>nul
if errorlevel 1 (
    echo Go was not found on PATH. Install Go 1.27 or newer from https://go.dev/dl/
    goto :fail
)

echo Building prbot.exe...
go build -o prbot.exe ./cmd/prbot
if errorlevel 1 (
    echo Build failed. If the error says "Access is denied", prbot.exe is still
    echo running from an earlier start: stop it with Ctrl+C in its window, then try again.
    goto :fail
)

echo Starting prbot. The web UI is at http://127.0.0.1:8080 unless -listen says otherwise.
echo Press Ctrl+C to stop.
rem Explicit path: cmd skips the current folder when NoDefaultCurrentDirectoryInExePath is set.
"%CD%\prbot.exe" %*
set "RC=%errorlevel%"
popd
exit /b %RC%

:fail
rem Undo the pushd (and free any drive letter it mapped), but only if it worked,
rem so a caller's own pushd stack is left alone.
if defined PUSHED popd
rem Keep the window open when the script was double-clicked from Explorer.
pause
exit /b 1
