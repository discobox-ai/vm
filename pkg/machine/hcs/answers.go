package hcs

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"fmt"
	"strings"
)

// The install medium's own files (ADR 26-11-14-816 §3): disco-vm writes them
// beside the retail ISO's boot files, in an ISO of its own.
const (
	// mediumDir holds the agent and the scripts; a script's presence there
	// is how the guest tells this medium from the retail DVD.
	mediumDir      = "disco-vm"
	peScriptName   = "pe.cmd"
	specializeName = "specialize.cmd"
	agentName      = "disco-vm.exe"
	// installDrive is the letter pe.cmd gives the retail DVD, which
	// InstallFrom names: in WinPE the DVDs' own letters follow no rule.
	installDrive = "Z"
)

// peScript runs in WinPE (windowsPE, RunSynchronous), before Setup reads
// the image. specializeScript runs in the new Windows as SYSTEM (specialize,
// RunSynchronous), and logs to C:\disco-vm\specialize.log.
//
//go:embed pe.cmd
var peScript []byte

//go:embed specialize.cmd
var specializeScript []byte

// answers is what the answer file varies by.
type answers struct {
	// Index is the edition's image in install.wim.
	Index int
	// WIM is the retail DVD's image file, under its root: sources\install.wim
	// or sources\install.esd.
	WIM      string
	User     string
	Password string
}

// answerFile is Setup's autounattend.xml (ADR 26-11-14-816 §5, §6): it wipes
// disk 0 into an ESP, an MSR, and Windows, installs the chosen edition from
// the retail DVD, and runs the specialize script, which installs the agent.
func answerFile(a answers) ([]byte, error) {
	if a.Index < 1 {
		return nil, fmt.Errorf("hcs: an edition index of %d", a.Index)
	}
	if a.User == "" {
		return nil, fmt.Errorf("hcs: an answer file with no user")
	}
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	// Each RunSynchronous finds the medium by its scripts, on any letter.
	find := func(script string) string {
		return fmt.Sprintf(`cmd.exe /c for %%d in (C D E F G H I J K L M N O P Q R S T U V W Y) do if exist %%d:\%s\%s call %%d:\%s\%s`, mediumDir, script, mediumDir, script)
	}
	user, password := esc(a.User), esc(a.Password)
	vars := strings.NewReplacer(
		"{{INDEX}}", fmt.Sprint(a.Index),
		"{{WIM}}", esc(installDrive+`:\`+strings.ReplaceAll(a.WIM, "/", `\`)),
		"{{PE}}", esc(find(peScriptName)),
		"{{SPECIALIZE}}", esc(find(specializeName)),
		"{{USER}}", user,
		"{{PASSWORD}}", password,
	)
	return []byte(vars.Replace(answerTemplate)), nil
}

// crlf gives a script cmd.exe's line endings; the repository keeps LF.
func crlf(b []byte) []byte {
	return []byte(strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n", "\r\n"))
}

const answerTemplate = `<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
  <settings pass="windowsPE">
    <component name="Microsoft-Windows-International-Core-WinPE" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <SetupUILanguage><UILanguage>en-US</UILanguage></SetupUILanguage>
      <InputLocale>en-US</InputLocale><SystemLocale>en-US</SystemLocale><UILanguage>en-US</UILanguage><UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <RunSynchronous>
        <RunSynchronousCommand wcm:action="add"><Order>1</Order><Path>{{PE}}</Path></RunSynchronousCommand>
      </RunSynchronous>
      <DiskConfiguration>
        <Disk wcm:action="add">
          <DiskID>0</DiskID>
          <WillWipeDisk>true</WillWipeDisk>
          <CreatePartitions>
            <CreatePartition wcm:action="add"><Order>1</Order><Type>EFI</Type><Size>300</Size></CreatePartition>
            <CreatePartition wcm:action="add"><Order>2</Order><Type>MSR</Type><Size>16</Size></CreatePartition>
            <CreatePartition wcm:action="add"><Order>3</Order><Type>Primary</Type><Extend>true</Extend></CreatePartition>
          </CreatePartitions>
          <ModifyPartitions>
            <ModifyPartition wcm:action="add"><Order>1</Order><PartitionID>1</PartitionID><Format>FAT32</Format><Label>System</Label></ModifyPartition>
            <ModifyPartition wcm:action="add"><Order>2</Order><PartitionID>3</PartitionID><Format>NTFS</Format><Label>Windows</Label><Letter>C</Letter></ModifyPartition>
          </ModifyPartitions>
        </Disk>
      </DiskConfiguration>
      <ImageInstall>
        <OSImage>
          <InstallFrom>
            <Path>{{WIM}}</Path>
            <MetaData wcm:action="add"><Key>/IMAGE/INDEX</Key><Value>{{INDEX}}</Value></MetaData>
          </InstallFrom>
          <InstallTo><DiskID>0</DiskID><PartitionID>3</PartitionID></InstallTo>
        </OSImage>
      </ImageInstall>
      <UserData>
        <ProductKey><Key></Key><WillShowUI>OnError</WillShowUI></ProductKey>
        <AcceptEula>true</AcceptEula>
      </UserData>
    </component>
  </settings>
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <ComputerName>*</ComputerName>
      <TimeZone>UTC</TimeZone>
    </component>
    <component name="Microsoft-Windows-Deployment" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <RunSynchronous>
        <RunSynchronousCommand wcm:action="add"><Order>1</Order><Path>{{SPECIALIZE}}</Path></RunSynchronousCommand>
      </RunSynchronous>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE><HideEULAPage>true</HideEULAPage><HideLocalAccountScreen>true</HideLocalAccountScreen><HideOnlineAccountScreens>true</HideOnlineAccountScreens><HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE><ProtectYourPC>3</ProtectYourPC></OOBE>
      <UserAccounts><LocalAccounts><LocalAccount wcm:action="add"><Name>{{USER}}</Name><DisplayName>{{USER}}</DisplayName><Group>Administrators</Group><Password><Value>{{PASSWORD}}</Value><PlainText>true</PlainText></Password></LocalAccount></LocalAccounts></UserAccounts>
      <AutoLogon><Enabled>true</Enabled><Username>{{USER}}</Username><LogonCount>2147483647</LogonCount><Password><Value>{{PASSWORD}}</Value><PlainText>true</PlainText></Password></AutoLogon>
    </component>
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <InputLocale>en-US</InputLocale><SystemLocale>en-US</SystemLocale><UILanguage>en-US</UILanguage><UserLocale>en-US</UserLocale>
    </component>
  </settings>
</unattend>
`
