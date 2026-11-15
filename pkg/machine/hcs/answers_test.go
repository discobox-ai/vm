package hcs

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

// The answer file is well-formed XML whatever the user and password hold,
// installs the chosen image from the retail DVD's fixed letter, and runs
// each script from the medium, on whatever letter it has.
func TestAnswerFile(t *testing.T) {
	b, err := answerFile(answers{Index: 6, WIM: "sources/install.wim", User: "a&b", Password: `<p"w'>`})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Settings []struct {
			Pass       string `xml:"pass,attr"`
			Components []struct {
				Name     string   `xml:"name,attr"`
				Commands []string `xml:"RunSynchronous>RunSynchronousCommand>Path"`
				Path     string   `xml:"ImageInstall>OSImage>InstallFrom>Path"`
				Index    string   `xml:"ImageInstall>OSImage>InstallFrom>MetaData>Value"`
				Users    []string `xml:"UserAccounts>LocalAccounts>LocalAccount>Name"`
				Password string   `xml:"AutoLogon>Password>Value"`
			} `xml:"component"`
		} `xml:"settings"`
	}
	if err := xml.NewDecoder(bytes.NewReader(b)).Decode(&doc); err != nil {
		t.Fatalf("the answer file is not XML: %v\n%s", err, b)
	}
	got := map[string]string{}
	for _, s := range doc.Settings {
		for _, c := range s.Components {
			for _, cmd := range c.Commands {
				got[s.Pass+" run"] = cmd
			}
			if c.Path != "" {
				got["path"], got["index"] = c.Path, c.Index
			}
			if len(c.Users) > 0 {
				got["user"], got["password"] = c.Users[0], c.Password
			}
		}
	}
	for key, want := range map[string]string{
		"path":     `Z:\sources\install.wim`,
		"index":    "6",
		"user":     "a&b",
		"password": `<p"w'>`,
	} {
		if got[key] != want {
			t.Errorf("%s is %q, want %q", key, got[key], want)
		}
	}
	for pass, script := range map[string]string{"windowsPE": peScriptName, "specialize": specializeName} {
		cmd := got[pass+" run"]
		if !strings.Contains(cmd, `%d:\disco-vm\`+script) || strings.Contains(cmd, " Z ") {
			t.Errorf("%s runs %q", pass, cmd)
		}
	}
}

func TestAnswerFileRefuses(t *testing.T) {
	for name, a := range map[string]answers{
		"no index": {WIM: "sources/install.wim", User: "u"},
		"no user":  {Index: 1, WIM: "sources/install.wim"},
	} {
		if _, err := answerFile(a); err == nil {
			t.Errorf("%s: written", name)
		}
	}
}

func TestCRLF(t *testing.T) {
	if got := string(crlf([]byte("a\nb\r\nc"))); got != "a\r\nb\r\nc" {
		t.Errorf("crlf gives %q", got)
	}
	for name, s := range map[string][]byte{"pe.cmd": peScript, "specialize.cmd": specializeScript} {
		if bytes.Contains(s, []byte("\r")) {
			t.Errorf("%s keeps CRs in the repository", name)
		}
	}
}
