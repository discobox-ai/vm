@echo off
rem disco-vm hcs install: runs as SYSTEM in the new Windows (specialize,
rem RunSynchronous), from the install medium (ADR 26-11-14-816 §6). It
rem installs the agent and makes, online, what the offline install made in
rem the image's SYSTEM hive. It logs to C:\disco-vm\specialize.log, which a
rem committed disk keeps.
setlocal
set LOG=C:\disco-vm\specialize.log
if not exist C:\disco-vm mkdir C:\disco-vm
echo started %DATE% %TIME%>> %LOG%
copy /y "%~dp0disco-vm.exe" C:\disco-vm\disco-vm.exe >> %LOG% 2>&1
if errorlevel 1 goto fail
rem The agent: disco-vm itself, an auto-start SYSTEM service, restarted 5 s
rem after a crash, three times a day.
sc.exe create disco-vm binPath= "\"C:\disco-vm\disco-vm.exe\" guest" start= auto obj= LocalSystem DisplayName= "disco-vm guest agent" >> %LOG% 2>&1
if errorlevel 1 goto fail
sc.exe failure disco-vm reset= 86400 actions= restart/5000/restart/5000/restart/5000 >> %LOG% 2>&1
if errorlevel 1 goto fail
rem The agent logs on as the image's user with LogonUser, which refuses a
rem blank password unless this is off.
reg add HKLM\SYSTEM\CurrentControlSet\Control\Lsa /v LimitBlankPasswordUse /t REG_DWORD /d 0 /f >> %LOG% 2>&1
if errorlevel 1 goto fail
rem No automatic device encryption: a committed disk must be sealed to
rem nothing, so every VM made from it can read it.
reg add HKLM\SYSTEM\CurrentControlSet\Control\BitLocker /v PreventDeviceEncryption /t REG_DWORD /d 1 /f >> %LOG% 2>&1
if errorlevel 1 goto fail
rem Remote Desktop, where the edition hosts it (not Home, whose EditionID
rem starts with Core), is how disco-vm gui shows the desktop. Its firewall
rem rules stay closed: the agent relays to it from inside the guest.
set EDITION=
for /f "tokens=3" %%e in ('reg query "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion" /v EditionID') do set EDITION=%%e
echo edition %EDITION%>> %LOG%
if /i "%EDITION:~0,4%"=="Core" goto done
reg add "HKLM\SYSTEM\CurrentControlSet\Control\Terminal Server" /v fDenyTSConnections /t REG_DWORD /d 0 /f >> %LOG% 2>&1
if errorlevel 1 goto fail
echo remote desktop on>> %LOG%
:done
echo done %DATE% %TIME%>> %LOG%
exit /b 0
:fail
echo failed %DATE% %TIME%>> %LOG%
exit /b 1
