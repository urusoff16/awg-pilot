@echo off
rem Dev run with the portable Go toolchain (UI only: without elevation the tunnel cannot start).
set "PATH=D:\CLAUDE_CODE\tools\go\bin;D:\CLAUDE_CODE\tools\gopath\bin;%PATH%"
set "GOPATH=D:\CLAUDE_CODE\tools\gopath"
set "GOTOOLCHAIN=local"
cd /d "%~dp0"
wails dev -noreload
