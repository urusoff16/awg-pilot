@echo off
rem Dev run with the portable Go toolchain (UI only: without elevation the tunnel cannot start).
set "PATH=C:\CLAUDE_CODE\tools\go\bin;C:\CLAUDE_CODE\tools\gopath\bin;%PATH%"
set "GOPATH=C:\CLAUDE_CODE\tools\gopath"
set "GOTOOLCHAIN=local"
cd /d "%~dp0"
wails dev -noreload
