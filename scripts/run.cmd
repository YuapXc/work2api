@echo off
setlocal
set "WORK2API_LAUNCHER=cmd"
rem Use the Windows-provided host; run.ps1 supports Windows PowerShell 5.1.
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0run.ps1" %*
set "work2api_exit=%errorlevel%"
if not "%work2api_exit%"=="0" (
  echo.
  echo Work2Api failed to start. Review the error above.
  pause
)
exit /b %work2api_exit%
