@echo off
rem disco-vm hcs install: runs in WinPE (windowsPE, RunSynchronous) before
rem Windows Setup reads its image (ADR 26-11-14-816 §3, §5). It finds the
rem retail DVD, the one with sources\install.wim and none of disco-vm's
rem files, and gives it the letter the answer file's InstallFrom names. Then
rem it lets Setup past the checks a VM cannot pass: its processor is the
rem host's, and HCS gives a VM no TPM without the Hyper-V role.
set SRC=
for %%d in (C D E F G H I J K L M N O P Q R S T U V W Y) do (
  if not exist %%d:\disco-vm\pe.cmd (
    if exist %%d:\sources\install.wim set SRC=%%d
    if exist %%d:\sources\install.esd set SRC=%%d
  )
)
if not defined SRC (
  echo disco-vm pe: no DVD holds sources\install.wim or install.esd
  exit /b 1
)
subst Z: %SRC%:\
if errorlevel 1 (
  echo disco-vm pe: subst Z: %SRC%:\ failed
  exit /b 1
)
echo disco-vm pe: the retail DVD is %SRC%:, given Z:
reg add HKLM\SYSTEM\Setup\LabConfig /v BypassTPMCheck /t REG_DWORD /d 1 /f >nul
reg add HKLM\SYSTEM\Setup\LabConfig /v BypassCPUCheck /t REG_DWORD /d 1 /f >nul
exit /b 0
