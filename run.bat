@echo off
rem Builds prbot.exe from source and starts it.
rem Any arguments are passed to the bot, for example:
rem   run.bat -listen 0.0.0.0:8080
rem   run.bat -data data-dev -listen 127.0.0.1:8087
setlocal

rem Run from the repository folder so the default "data" folder is the one here.
cd /d "%~dp0"

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
rem Full path: cmd skips the current folder when NoDefaultCurrentDirectoryInExePath is set.
"%~dp0prbot.exe" %*
exit /b %errorlevel%

:fail
rem Keep the window open when the script was double-clicked from Explorer.
pause
exit /b 1
