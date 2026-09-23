package build

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

const baseSpec = `
name: discobox/base
tag: "${VERSION}"
from:
  image: ${OS_IMAGE}
args:
  VERSION: "1"
  OS_IMAGE: discobox/windows:11
env:
  GREETING: hello ${VERSION}
layers:
  - name: toolchains
    steps:
      - run: winget install --id Git.Git
        when: {os: windows}
      - run: brew install git
        when: {os: [darwin]}
      - run: echo $env:PATH ${UNDECLARED} ${VERSION}
  - name: mac-only
    when: {os: darwin}
    steps:
      - reboot: true
`

func TestParseAndPlanPerOS(t *testing.T) {
	spec, err := Parse([]byte(baseSpec))
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]string{"VERSION": "2", "OS_IMAGE": "x"}
	spec.Name, spec.Tag = expand(spec.Name, args), expand(spec.Tag, args)
	if spec.Ref() != "discobox/base:2" {
		t.Fatalf("ref = %q", spec.Ref())
	}

	win, err := plan(spec, args, t.TempDir(), machine.Windows)
	if err != nil {
		t.Fatal(err)
	}
	if len(win.Layers) != 1 || len(win.Layers[0].Steps) != 2 {
		t.Fatalf("windows plan: %+v", win.Layers)
	}
	if !slices.Equal(win.Skipped, []string{"mac-only"}) {
		t.Fatalf("skipped = %v", win.Skipped)
	}
	script := win.Layers[0].Steps[1].Argv[len(win.Layers[0].Steps[1].Argv)-1]
	// Declared args are substituted; the shell's own variables and undeclared
	// names are left for the guest.
	if !strings.HasSuffix(script, "echo $env:PATH ${UNDECLARED} 2") {
		t.Fatalf("script = %q", script)
	}
	if !strings.HasPrefix(script, windowsPrelude) {
		t.Fatalf("default Windows shell should get the prelude: %q", script)
	}
	if !slices.Contains(win.Layers[0].Steps[1].Env, "GREETING=hello 2") {
		t.Fatalf("env = %v", win.Layers[0].Steps[1].Env)
	}

	mac, err := plan(spec, args, t.TempDir(), machine.Darwin)
	if err != nil {
		t.Fatal(err)
	}
	if len(mac.Layers) != 2 || !slices.Equal(mac.Layers[0].Steps[0].Argv, []string{"/bin/zsh", "-l", "-c", "brew install git"}) {
		t.Fatalf("darwin plan: %+v", mac.Layers)
	}
}

func TestParseRejects(t *testing.T) {
	for name, spec := range map[string]string{
		"unknown key":  "name: a\nfrom: {image: b}\nlayrs: []\n",
		"two bases":    "name: a\nfrom: {image: b, install: {os: windows, media: x}}\n",
		"no base":      "name: a\nfrom: {}\n",
		"bad os":       "name: a\nfrom: {install: {os: plan9, media: x}}\n",
		"two kinds":    "name: a\nfrom: {image: b}\nlayers: [{name: l, steps: [{run: x, reboot: true}]}]\n",
		"empty layer":  "name: a\nfrom: {image: b}\nlayers: [{name: l, steps: []}]\n",
		"dup layer":    "name: a\nfrom: {image: b}\nlayers: [{name: l, steps: [{run: x}]}, {name: l, steps: [{run: y}]}]\n",
		"bad timeout":  "name: a\nfrom: {image: b}\nlayers: [{name: l, steps: [{run: x, timeout: soon}]}]\n",
		"bad when os":  "name: a\nfrom: {image: b}\nlayers: [{name: l, when: {os: beos}, steps: [{run: x}]}]\n",
		"copy no dest": "name: a\nfrom: {image: b}\nlayers: [{name: l, steps: [{copy: {src: x}}]}]\n",
	} {
		if _, err := Parse([]byte(spec)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestLayerKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := Parse([]byte("name: a\nfrom: {image: b}\nlayers: [{name: l, steps: [{copy: {src: a.txt, dst: /x}}, {name: say, run: echo hi}]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	key := func() string {
		p, err := plan(spec, nil, dir, machine.Linux)
		if err != nil {
			t.Fatal(err)
		}
		return layerKey("parent", "fake", machine.Linux, p.Layers[0], "")
	}
	first := key()
	if key() != first {
		t.Fatal("key is not deterministic")
	}
	spec.Layers[0].Steps[1].Name = "renamed"
	if key() != first {
		t.Fatal("renaming a step changed the key")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if key() == first {
		t.Fatal("changing a copied file did not change the key")
	}
}

func TestCopyMustStayInContext(t *testing.T) {
	spec, err := Parse([]byte("name: a\nfrom: {image: b}\nlayers: [{name: l, steps: [{copy: {src: ../secret, dst: /x}}]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan(spec, nil, t.TempDir(), machine.Linux); err == nil {
		t.Fatal("a copy from outside the context was planned")
	}
}
