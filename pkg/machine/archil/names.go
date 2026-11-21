package archil

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A VM is the sandbox named for it, dvm-<pool>--<name>, which Archil keeps
// unique (ADR 26-11-03-606), and its data volume is the disk
// dvm-<pool>--<name>--data. A name has no "--" in it, so neither is
// mistaken for another VM's. Nothing else is kept: Archil holds every fact
// the driver reports.

const (
	namePrefix = "dvm-"
	dataSuffix = "--data"
	// maxSandboxName is the longest name Archil gives a sandbox (its OpenAPI
	// document's maxLength). A data disk's name is six longer, well inside
	// a disk's 100.
	maxSandboxName = 63
)

// prefix starts the name of each of the pool's sandboxes and disks.
func (d *Driver) prefix() string { return machine.ResourcePrefix(namePrefix, d.pool) }

// sandboxName is the sandbox of a VM.
func (d *Driver) sandboxName(name string) string { return d.prefix() + name }

// dataDiskName is the data volume of a VM.
func (d *Driver) dataDiskName(name string) string { return d.sandboxName(name) + dataSuffix }

// nameOf is the VM a sandbox is, or "" for one not the pool's: a name
// that does not pass machine.ValidateName, such as one an earlier driver made
// (dvm-<pool>-<key>), is none.
func (d *Driver) nameOf(sandboxName string) string {
	name, ok := strings.CutPrefix(sandboxName, d.prefix())
	if !ok || machine.ValidateName(name) != nil {
		return ""
	}
	return name
}

// nameOfDisk is the VM a data disk is the volume of, or "" for a disk that
// is none of the pool's VMs'.
func (d *Driver) nameOfDisk(diskName string) string {
	base, ok := strings.CutSuffix(diskName, dataSuffix)
	if !ok {
		return ""
	}
	return d.nameOf(base)
}

// checkName refuses a VM name whose sandbox's name would be longer than
// Archil takes.
func (d *Driver) checkName(name string) error {
	if n := len(d.sandboxName(name)); n > maxSandboxName {
		return fmt.Errorf("archil: VM name %q is too long for pool %s: its sandbox's name would be %d characters, and Archil's are at most %d (a pool and a name together are at most %d)",
			name, d.pool, n, maxSandboxName, maxSandboxName-len(d.sandboxName(""))+len(d.pool))
	}
	return nil
}

// A sandbox the driver makes carries labels naming its pool, its VM, and
// the VM it was copied from, as a Docker driver's container does, so a
// pool's sandboxes are found in Archil by label (label=ai.discobox.vm.pool=
// <pool>), and a listing reports a copy's From, which Archil does not keep.
// The name stays the VM's identity: Archil sets labels only when it makes
// a sandbox, so one made before they were set has none and never will.
const (
	labelPool     = "ai.discobox.vm.pool"
	labelInstance = "ai.discobox.vm.instance"
	labelFrom     = "ai.discobox.vm.from"
)

// labels are the labels of the sandbox of a VM spec makes. A pool and a VM
// name are within Archil's 63 characters of a value, and of its alphabet.
func (d *Driver) labels(spec machine.InstanceSpec) map[string]string {
	labels := map[string]string{labelPool: d.pool, labelInstance: spec.Name}
	if spec.From != nil {
		labels[labelFrom] = spec.From.Name
	}
	return labels
}

// filter has Archil list a VM's sandbox, by name, or with name "" every
// sandbox under the pool's prefix, labeled or not. The listing's caller
// still reads each name (nameOf).
func (d *Driver) filter(name string) url.Values {
	if name != "" {
		return url.Values{"name": {d.sandboxName(name)}}
	}
	return url.Values{"name_prefix": {d.prefix()}}
}
