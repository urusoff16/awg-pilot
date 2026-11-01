Unicode true

####
## Please note: Template replacements don't work in this file. They are provided with default defines like
## mentioned underneath.
## If the keyword is not defined, "wails_tools.nsh" will populate them with the values from ProjectInfo.
## If they are defined here, "wails_tools.nsh" will not touch them. This allows to use this project.nsi manually
## from outside of Wails for debugging and development of the installer.
##
## For development first make a wails nsis build to populate the "wails_tools.nsh":
## > wails build --target windows/amd64 --nsis
## Then you can call makensis on this file with specifying the path to your binary:
## For a AMD64 only installer:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\app.exe
## For a ARM64 only installer:
## > makensis -DARG_WAILS_ARM64_BINARY=..\..\bin\app.exe
## For a installer with both architectures:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\app-amd64.exe -DARG_WAILS_ARM64_BINARY=..\..\bin\app-arm64.exe
####
## The following information is taken from the ProjectInfo file, but they can be overwritten here.
####
## !define INFO_PROJECTNAME    "MyProject" # Default "{{.Name}}"
## !define INFO_COMPANYNAME    "MyCompany" # Default "{{.Info.CompanyName}}"
## !define INFO_PRODUCTNAME    "MyProduct" # Default "{{.Info.ProductName}}"
## !define INFO_PRODUCTVERSION "1.0.0"     # Default "{{.Info.ProductVersion}}"
## !define INFO_COPYRIGHT      "Copyright" # Default "{{.Info.Copyright}}"
###
## !define PRODUCT_EXECUTABLE  "Application.exe"      # Default "${INFO_PROJECTNAME}.exe"
## !define UNINST_KEY_NAME     "UninstKeyInRegistry"  # Default "${INFO_COMPANYNAME}${INFO_PRODUCTNAME}"
####
## !define REQUEST_EXECUTION_LEVEL "admin"            # Default "admin"  see also https://nsis.sourceforge.io/Docs/Chapter4.html
####
## Include the wails tools
####
!include "wails_tools.nsh"

# The version information for this two must consist of 4 parts
VIProductVersion "${INFO_PRODUCTVERSION}.0"
VIFileVersion    "${INFO_PRODUCTVERSION}.0"

VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} Installer"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

# Enable HiDPI support. https://nsis.sourceforge.io/Reference/ManifestDPIAware
ManifestDPIAware true

!include "MUI.nsh"

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
# !define MUI_WELCOMEFINISHPAGE_BITMAP "resources\leftimage.bmp" #Include this to add a bitmap on the left side of the Welcome Page. Must be a size of 164x314
!define MUI_FINISHPAGE_NOAUTOCLOSE # Wait on the INSTFILES page so the user can take a look into the details of the installation steps
!define MUI_ABORTWARNING # This will warn the user if they exit from the installer.

!insertmacro MUI_PAGE_WELCOME # Welcome to the installer page.
# !insertmacro MUI_PAGE_LICENSE "resources\eula.txt" # Adds a EULA page to the installer
# No folder choice: the service runs as LocalSystem from $INSTDIR, so it must stay in Program Files,
# where ordinary users cannot replace the executable. It also keeps the uninstaller from deleting
# anything but its own files.
!insertmacro MUI_PAGE_INSTFILES # Installing page.
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Запустить AWG Pilot"
!define MUI_FINISHPAGE_RUN_FUNCTION LaunchAsUser # the window must not inherit the installer's admin rights
!insertmacro MUI_PAGE_FINISH # Finished installation page.

!insertmacro MUI_UNPAGE_INSTFILES # Uinstalling page

!insertmacro MUI_LANGUAGE "Russian" # Set the Language of the installer

## The following two statements can be used to sign the installer and the uninstaller. The path to the binaries are provided in %1
#!uninstfinalize 'signtool --file "%1"'
#!finalize 'signtool --file "%1"'

Name "${INFO_PRODUCTNAME}"
OutFile "..\..\bin\${INFO_PROJECTNAME}-${ARCH}-installer.exe" # Name of the installer's file.
# The tunnel runs in a machine-wide service, so AWG Pilot always installs per machine.
InstallDir "$PROGRAMFILES64\${INFO_PRODUCTNAME}"
ShowInstDetails show # This will always show the installation details.

!define SERVICE_NAME "AWGPilot"

Function .onInit
   !insertmacro wails.checkArchitecture
   # Ignore /D=: the service runs as LocalSystem from here, so it must stay in Program Files.
   StrCpy $INSTDIR "$PROGRAMFILES64\${INFO_PRODUCTNAME}"
FunctionEnd

# Explorer starts the window as the signed-in user (not elevated like this installer).
Function LaunchAsUser
    Exec '"$WINDIR\explorer.exe" "$INSTDIR\${PRODUCT_EXECUTABLE}"'
FunctionEnd

# System tools print in the console code page; run them silently and log our own text instead.
!macro StopAppAndService
    DetailPrint "Остановка службы и окна AWG Pilot…"
    nsExec::Exec '"$SYSDIR\sc.exe" stop ${SERVICE_NAME}'
    Pop $0
    Sleep 2000
    nsExec::Exec '"$SYSDIR\taskkill.exe" /IM ${PRODUCT_EXECUTABLE} /F'
    Pop $0
    Sleep 500
    # versions up to 0.4 started elevated through a Task Scheduler entry
    nsExec::Exec '"$SYSDIR\schtasks.exe" /Delete /TN "AWG Pilot" /F'
    Pop $0
!macroend

Section
    !insertmacro wails.setShellContext

    !insertmacro wails.webview2runtime

    SetOutPath $INSTDIR

    # An update must replace the running exe: stop the service and close the window first.
    !insertmacro StopAppAndService

    !insertmacro wails.files
    File "..\..\bin\wintun.dll" # WireGuard LLC wintun 0.14.1, required for the tunnel adapter

    # (Re)register the service that owns the tunnel, routes and firewall rules.
    DetailPrint "Регистрация службы «AWG Pilot VPN»…"
    nsExec::Exec '"$SYSDIR\sc.exe" delete ${SERVICE_NAME}'
    Pop $0
    Sleep 1000
    nsExec::Exec '"$SYSDIR\sc.exe" create ${SERVICE_NAME} binPath= "\"$INSTDIR\${PRODUCT_EXECUTABLE}\" --service" start= auto DisplayName= "AWG Pilot VPN"'
    Pop $0
    StrCmp $0 "0" +3
        DetailPrint "Не удалось зарегистрировать службу (код $0)."
        MessageBox MB_ICONEXCLAMATION "Служба AWG Pilot VPN не зарегистрирована (код $0). Без неё VPN работать не будет — запустите установку ещё раз."
    nsExec::Exec '"$SYSDIR\sc.exe" description ${SERVICE_NAME} "Туннель AmneziaWG для программы AWG Pilot"'
    Pop $0
    nsExec::Exec '"$SYSDIR\sc.exe" failure ${SERVICE_NAME} reset= 86400 actions= restart/5000/restart/5000/none/0'
    Pop $0
    DetailPrint "Запуск службы…"
    nsExec::Exec '"$SYSDIR\sc.exe" start ${SERVICE_NAME}'
    Pop $0
    StrCmp $0 "0" +3
        DetailPrint "Служба не запустилась (код $0) — она запустится при следующем входе в Windows."
        Goto +2
        DetailPrint "Служба «AWG Pilot VPN» работает."

    CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    CreateShortCut "$DESKTOP\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"

    !insertmacro wails.associateFiles
    !insertmacro wails.associateCustomProtocols

    !insertmacro wails.writeUninstaller
SectionEnd

Section "uninstall"
    !insertmacro StopAppAndService
    DetailPrint "Удаление службы «AWG Pilot VPN»…"
    nsExec::Exec '"$SYSDIR\sc.exe" delete ${SERVICE_NAME}'
    Pop $0
    Sleep 1000

    # Per-user data lives in the current user's profile, not the all-users shell context.
    SetShellVarContext current
    DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "AWG Pilot" # autostart entry
    RMDir /r "$APPDATA\${PRODUCT_EXECUTABLE}" # WebView2 data
    MessageBox MB_YESNO|MB_ICONQUESTION "Удалить сохранённый ключ vpn:// и настройки AWG Pilot?$\r$\n$\r$\nНет — оставить их для повторной установки." /SD IDNO IDNO keepdata
        RMDir /r "$APPDATA\AWGPilot"
    keepdata:

    !insertmacro wails.setShellContext

    RMDir /r "$AppData\${PRODUCT_EXECUTABLE}" # Remove the WebView2 DataPath

    # Only our own files: never a recursive delete of the install folder.
    Delete "$INSTDIR\${PRODUCT_EXECUTABLE}"
    Delete "$INSTDIR\wintun.dll"
    Delete "$INSTDIR\awgpilot-*.bak.exe" # copies left by manual updates
    RMDir $INSTDIR
    ReadEnvStr $1 ProgramData # the service's state: lockdown flag and the "only through VPN" list
    Delete "$1\AWGPilot\lockdown"
    Delete "$1\AWGPilot\vpn-only-apps.json"
    RMDir "$1\AWGPilot"

    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"
    Delete "$DESKTOP\${INFO_PRODUCTNAME}.lnk"

    !insertmacro wails.unassociateFiles
    !insertmacro wails.unassociateCustomProtocols

    !insertmacro wails.deleteUninstaller
SectionEnd
