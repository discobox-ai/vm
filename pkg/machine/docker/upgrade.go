package docker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// An upgrade (ADR 26-11-03-782) gives a VM a new container, of a committed
// VM's image or of one readied from the base, that mounts the VM's same
// directories and carries its labels. The new container is made under
// upgradeName, then the old one is removed, then the new one is renamed
// into place. Whatever step it stops at, the VM is found under its name: by
// its old container until that is removed, then by the new one, which Boot
// refuses until an Upgrade again with the same from renames it, and which
// Destroy removes with the VM's directories.

const (
	// upgradeSuffix ends the name of an upgrade's new container until it is
	// renamed into place. No VM's name has "--" in it.
	upgradeSuffix = "--upgrade"
	// labelReplaces is the IDs of the containers a VM's container replaced,
	// newest first, so an Instance from before an upgrade still names it.
	labelReplaces = "ai.discobox.vm.replaces"
	// labelReplacedImage is the image the container an upgrade replaced
	// ran, removed once nothing needs it, by whichever Upgrade finishes it.
	labelReplacedImage = "ai.discobox.vm.replaced-image"
)

// upgradeStep is called after each step of an upgrade, and fails it where
// it returns an error. Tests set it to interrupt one.
var upgradeStep = func(string) error { return nil }

// upgradeName is the name of an upgrade's new container.
func upgradeName(project, name string) string { return containerName(project, name) + upgradeSuffix }

// upgrading reports whether c is an upgrade's new container, not yet
// renamed into place.
func upgrading(c containerInfo) bool { return strings.HasSuffix(c.Name, upgradeSuffix) }

// replaces reports whether c replaced the container id in an upgrade.
func replaces(c containerInfo, id string) bool {
	return id != "" && slices.Contains(strings.Split(c.Config.Labels[labelReplaces], ","), id)
}

// vmContainers is the containers of the VM inst names, its own first and an
// upgrade's new one after: none where neither is inst's, by its ID or as
// one an upgrade replaced.
func (d *Driver) vmContainers(ctx context.Context, inst machine.Instance) ([]containerInfo, error) {
	var out []containerInfo
	for _, ref := range []string{containerName(d.project, inst.Name), upgradeName(d.project, inst.Name)} {
		c, err := d.api.inspectContainer(ctx, ref)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if d.managed(c.Config.Labels) && c.Config.Labels[labelInstance] == inst.Name {
			out = append(out, c)
		}
	}
	for _, c := range out {
		if c.ID == inst.ID || replaces(c, inst.ID) {
			return out, nil
		}
	}
	return nil, nil
}

// resolve is the container a VM runs as: its own, or, while an upgrade is
// caught between its containers, the new one. The daemon's not-found error
// is where it has none.
func (d *Driver) resolve(ctx context.Context, inst machine.Instance) (containerInfo, error) {
	if machine.ValidateName(inst.Name) != nil {
		return containerInfo{}, errNoContainer
	}
	cs, err := d.vmContainers(ctx, inst)
	if err != nil {
		return containerInfo{}, err
	}
	if len(cs) == 0 {
		return containerInfo{}, errNoContainer
	}
	return cs[0], nil
}

// whole is the container a VM runs as, refused while an upgrade of it is
// caught between its containers: from the moment the upgrade's new
// container is made, whether or not the old one is gone yet, until it is
// renamed into place. Nothing runs or commits the old container under an
// upgrade that would remove it.
func (d *Driver) whole(ctx context.Context, inst machine.Instance) (containerInfo, error) {
	if machine.ValidateName(inst.Name) != nil {
		return containerInfo{}, errNoContainer
	}
	cs, err := d.vmContainers(ctx, inst)
	if err != nil {
		return containerInfo{}, err
	}
	if len(cs) == 0 {
		return containerInfo{}, errNoContainer
	}
	if slices.ContainsFunc(cs, upgrading) {
		return containerInfo{}, fmt.Errorf("docker: an upgrade of %s is unfinished; run upgrade again with the same --from, or remove the VM", inst.Name)
	}
	return cs[0], nil
}

// Upgrade gives the stopped VM inst a new container of the committed VM
// from's image, or of one readied from the base, mounting its same
// directories, and removes its old one.
func (d *Driver) Upgrade(ctx context.Context, inst machine.Instance, from *machine.Instance) (machine.Instance, error) {
	if err := refuseCommitted(inst); err != nil {
		return machine.Instance{}, err
	}
	if machine.ValidateName(inst.Name) != nil {
		return machine.Instance{}, fmt.Errorf("instance %q: %w", inst.Name, machine.ErrNotFound)
	}
	cs, err := d.vmContainers(ctx, inst)
	if err != nil {
		return machine.Instance{}, err
	}
	if len(cs) == 0 {
		return machine.Instance{}, fmt.Errorf("docker: VM %s: %w", inst.Name, machine.ErrNotFound)
	}
	var old, next *containerInfo
	for i := range cs {
		if upgrading(cs[i]) {
			next = &cs[i]
		} else {
			old = &cs[i]
		}
	}
	if old != nil && old.State.Running {
		return machine.Instance{}, fmt.Errorf("docker: VM %s is running; stop it first", inst.Name)
	}
	fromName := ""
	if from != nil {
		if from.GuestOS != "" && from.GuestOS != machine.Linux {
			return machine.Instance{}, fmt.Errorf("docker: VM %s is a linux guest, and %s a %s one", inst.Name, from.Name, from.GuestOS)
		}
		fromName = from.Name
	}
	if next != nil {
		// A new container is whole once it is made, in one call, so an
		// upgrade that made it is finished with it, whatever has become of
		// the committed VM it was made from since.
		if got := next.Config.Labels[labelFrom]; got != fromName {
			return machine.Instance{}, fmt.Errorf("docker: an upgrade of %s from %q is unfinished; finish it with the same from, or remove the VM", inst.Name, got)
		}
		if old == nil {
			return d.finishUpgrade(ctx, inst.Name, next.ID, "", next.Config.Labels[labelReplacedImage])
		}
		return d.finishUpgrade(ctx, inst.Name, next.ID, old.ID, old.Image)
	}
	img := ""
	switch {
	case from != nil:
		src, found, err := d.committed(ctx, from.Name)
		if err != nil {
			return machine.Instance{}, err
		}
		if !found || (from.ID != "" && src.ID != from.ID && image(*from)) {
			return machine.Instance{}, fmt.Errorf("docker: committed VM %s: %w", from.Name, machine.ErrNotFound)
		}
		img = src.ID
	case d.base == "":
		return machine.Instance{}, fmt.Errorf("docker: a fresh root needs a base image, which is the driver's configuration: set %s to one, such as debian:trixie: %w", BaseEnv, machine.ErrUnsupported)
	}
	if old == nil {
		return machine.Instance{}, fmt.Errorf("docker: VM %s: %w", inst.Name, machine.ErrNotFound)
	}
	base := ""
	if img == "" {
		base = baseTag(d.project, inst.Name) + fsutil.RandomHex(3)
		if err := d.ready(ctx, d.base, inst.Name, base); err != nil {
			return machine.Instance{}, err
		}
		readied, err := d.api.inspectImage(ctx, baseRepo+":"+base)
		if err != nil {
			d.removeImage(base)
			return machine.Instance{}, fmt.Errorf("docker: %s:%s, readied for %s: %w", baseRepo, base, inst.Name, err)
		}
		img = readied.ID
	}
	labels := maps.Clone(old.Config.Labels)
	labels[labelFrom] = fromName
	labels[labelReplaces] = strings.TrimSuffix(old.ID+","+labels[labelReplaces], ",")
	labels[labelReplacedImage] = old.Image
	data, cache := labels[labelData] != "", labels[labelCache] != ""
	// A VM from before ADR 26-11-03-782 keeps its volumes in named volumes,
	// copied into its directories now, and removed once its old container
	// is.
	for v, has := range map[machine.Volume]bool{machine.VolumeData: data, machine.VolumeCache: cache} {
		if vol := oldVolume(*old, v); has && vol != "" {
			if _, err := d.runDirs(ctx, dirsRequest{Op: opMigrate, Name: inst.Name, Volume: string(v)}, vol); err != nil {
				d.removeImage(base)
				return machine.Instance{}, err
			}
		}
	}
	id, err := d.api.createContainer(ctx, upgradeName(d.project, inst.Name), d.vmConfig(img, inst.Name, labels, d.dirMounts(inst.Name, data, cache)))
	if err != nil {
		d.removeImage(base)
		return machine.Instance{}, fmt.Errorf("docker: create %s: %w", upgradeName(d.project, inst.Name), err)
	}
	if err := upgradeStep("created"); err != nil {
		return machine.Instance{}, err
	}
	// The VM was found stopped before a base was readied, which takes
	// minutes; one started meanwhile is not removed under itself. From here
	// on Boot refuses it (whole).
	if c, err := d.api.inspectContainer(ctx, old.ID); err != nil || c.State.Running {
		d.removeContainer(id)
		d.removeImage(base)
		if err == nil {
			err = fmt.Errorf("docker: VM %s was started while it was upgraded; stop it and upgrade it again", inst.Name)
		}
		return machine.Instance{}, err
	}
	return d.finishUpgrade(ctx, inst.Name, id, old.ID, old.Image)
}

// finishUpgrade removes the old container, where there is one, renames the
// new one into place, and removes what the old one alone needed: its named
// volumes from before ADR 26-11-03-782, and its image, where nothing else
// needs it.
func (d *Driver) finishUpgrade(ctx context.Context, name, next, old, oldImage string) (machine.Instance, error) {
	if old != "" {
		if err := d.api.removeContainer(ctx, old); err != nil {
			return machine.Instance{}, fmt.Errorf("docker: remove %s: %w", containerName(d.project, name), err)
		}
	}
	if err := upgradeStep("removed"); err != nil {
		return machine.Instance{}, err
	}
	if err := d.api.rename(ctx, next, containerName(d.project, name)); err != nil {
		return machine.Instance{}, fmt.Errorf("docker: rename %s: %w", upgradeName(d.project, name), err)
	}
	if err := d.removeOldVolumes(ctx, name); err != nil {
		return machine.Instance{}, err
	}
	if err := d.removeImages(ctx, oldImage, name); err != nil {
		return machine.Instance{}, err
	}
	c, err := d.api.inspectContainer(ctx, next)
	if err != nil {
		return machine.Instance{}, fmt.Errorf("docker: container %s: %w", containerName(d.project, name), err)
	}
	return d.instance(c), nil
}
