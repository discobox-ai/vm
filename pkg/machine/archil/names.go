package archil

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// A VM is the live sandbox its labels name (ADR 26-11-24-905): its pool's
// and its own. Its sandbox is named dvm-<pool>--<name> when a create makes
// it, so Archil refuses a second create of the name, and
// dvm-<pool>--<name>--<hex> when an upgrade makes it again; its data volume
// is the disk dvm-<pool>--<name>--data. A name has no "--" in it, so none of
// them is mistaken for another VM's. Nothing else is kept: Archil holds
// every fact the driver reports.

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

// nameOfDisk is the VM a data disk is the volume of, or "" for a disk that
// is none of the pool's VMs'.
func (d *Driver) nameOfDisk(diskName string) string {
	base, ok := strings.CutSuffix(diskName, dataSuffix)
	if !ok {
		return ""
	}
	name, ok := strings.CutPrefix(base, d.prefix())
	if !ok || machine.ValidateName(name) != nil {
		return ""
	}
	return name
}

// upgradeName is the name of an upgrade's new sandbox: the VM's sandbox
// name, cut to fit, and a random end after "--", which no VM name has.
func (d *Driver) upgradeName(name string) string {
	end := "--" + fsutil.RandomHex(2)
	base := d.sandboxName(name)
	if len(base)+len(end) > maxSandboxName {
		base = base[:maxSandboxName-len(end)]
	}
	return base + end
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
// the VM it was copied from, as a Docker driver's container does: the pool
// and the VM are what the sandbox is (ADR 26-11-24-905 §1), and From is
// reported from its label, since Archil does not keep it. An upgrade's new
// sandbox also names the sandbox it replaces, and is not the VM while that
// one lives (§3). Archil sets labels only when it makes a sandbox.
const (
	labelPool     = "ai.discobox.vm.pool"
	labelInstance = "ai.discobox.vm.instance"
	labelFrom     = "ai.discobox.vm.from"
	labelReplaces = "ai.discobox.vm.replaces"
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

// filter has Archil list the sandboxes of a VM by its labels, or with name
// "" every sandbox of the pool.
func (d *Driver) filter(name string) url.Values {
	q := url.Values{"label": {labelPool + "=" + d.pool}}
	if name != "" {
		q.Add("label", labelInstance+"="+name)
	}
	return q
}

// vms picks the sandbox that is each VM of the pool from sandboxes Archil
// listed (ADR 26-11-24-905 §3): of the live ones labeled for it, those an
// upgrade made to replace one still live are not, and of the rest the oldest,
// by creation, then ID, is.
func (d *Driver) vms(all []sandbox) map[string]sandbox {
	alive := map[string]bool{}
	for _, sb := range all {
		if live(sb) {
			alive[sb.ID] = true
		}
	}
	vms := map[string]sandbox{}
	for _, sb := range all {
		name := sb.Labels[labelInstance]
		if !live(sb) || sb.Labels[labelPool] != d.pool || machine.ValidateName(name) != nil || alive[sb.Labels[labelReplaces]] {
			continue
		}
		if cur, ok := vms[name]; !ok || older(sb, cur) {
			vms[name] = sb
		}
	}
	return vms
}

// older reports whether a was made before b, or with b and an ID before it.
func older(a, b sandbox) bool {
	if c := a.Created.Compare(b.Created); c != 0 {
		return c < 0
	}
	return a.ID < b.ID
}
