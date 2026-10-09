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
		"name":         "name: a\nfrom: {image: b}\n",
		"tag":          "tag: v1\nfrom: {image: b}\n",
		"unknown key":  "from: {image: b}\nlayrs: []\n",
		"two bases":    "from: {image: b, install: {os: windows, media: x}}\n",
		"no base":      "from: {}\n",
		"bad os":       "from: {install: {os: plan9, media: x}}\n",
		"two kinds":    "from: {image: b}\nlayers: [{name: l, steps: [{run: x, reboot: true}]}]\n",
		"empty layer":  "from: {image: b}\nlayers: [{name: l, steps: []}]\n",
		"dup layer":    "from: {image: b}\nlayers: [{name: l, steps: [{run: x}]}, {name: l, steps: [{run: y}]}]\n",
		"bad timeout":  "from: {image: b}\nlayers: [{name: l, steps: [{run: x, timeout: soon}]}]\n",
		"bad when os":  "from: {image: b}\nlayers: [{name: l, when: {os: beos}, steps: [{run: x}]}]\n",
		"copy no dest": "from: {image: b}\nlayers: [{name: l, steps: [{copy: {src: x}}]}]\n",
		"service 0":    "from: {image: b}\nservice: {port: 0}\n",
		"service big":  "from: {image: b}\nservice: {port: 70000}\n",
		"agent port":   "from: {image: b}\nservice: {port: 7300}\n",
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
	spec, err := Parse([]byte("from: {image: b}\nlayers: [{name: l, steps: [{copy: {src: a.txt, dst: /x}}, {name: say, run: echo hi}]}]\n"))
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
	spec, err := Parse([]byte("from: {image: b}\nlayers: [{name: l, steps: [{copy: {src: ../secret, dst: /x}}]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan(spec, nil, t.TempDir(), machine.Linux); err == nil {
		t.Fatal("a copy from outside the context was planned")
	}
}

// The service is recorded on the last layer the build makes, so it is part of
// that layer's key only, and a spec that makes no layer cannot declare one.
func TestServiceOnLastLayer(t *testing.T) {
	spec, err := Parse([]byte("from: {image: b}\nservice: {port: 8080}\nlayers: [{name: a, steps: [{run: x}]}, {name: b, steps: [{run: y}]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan(spec, nil, t.TempDir(), machine.Linux)
	if err != nil {
		t.Fatal(err)
	}
	if p.Layers[0].Service != 0 || p.Layers[1].Service != 8080 {
		t.Fatalf("services %d, %d; want 0, 8080", p.Layers[0].Service, p.Layers[1].Service)
	}
	without := p.Layers[1]
	without.Service = 0
	if layerKey("parent", "fake", machine.Linux, p.Layers[1], "") == layerKey("parent", "fake", machine.Linux, without, "") {
		t.Fatal("the service did not change the last layer's key")
	}

	spec, err = Parse([]byte("from: {image: b}\nservice: {port: 8080}\nlayers: [{name: a, when: {os: windows}, steps: [{run: x}]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan(spec, nil, t.TempDir(), machine.Linux); err == nil || !strings.Contains(err.Error(), "nothing in this spec makes one") {
		t.Fatalf("plan of a service with no layer: %v", err)
	}
}
