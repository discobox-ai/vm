package docker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A committed VM (ADR 26-11-03-606) is a Docker image in vmRepo tagged
// `<pool>--<name>`, committed from the VM's stopped container and labeled
// with its name, pool, size, the committed VM it came from, and
// labelCommitted. Its container is removed once it is committed. A VM is
// the pool's container dvm-<pool>--<name>, or that image; a name
// whose image is there is the committed VM's, whatever container a failed
// commit left.

// vmTag is a committed VM's Docker image: vmRepo:<pool>--<name>.
func vmTag(pool, name string) string {
	return vmRepo + ":" + machine.ResourcePrefix("", pool) + name
}

// committed is the pool's committed VM of a name, found false where its
// Docker image is not there or is not one.
func (d *Driver) committed(ctx context.Context, name string) (machine.Instance, bool, error) {
	img, err := d.api.inspectImage(ctx, vmTag(d.pool, name))
	if isNotFound(err) {
		return machine.Instance{}, false, nil
	}
	if err != nil {
		return machine.Instance{}, false, fmt.Errorf("docker: image %s: %w", vmTag(d.pool, name), err)
	}
	labels := img.Config.Labels
	if labels[labelCommitted] == "" || labels[labelPool] != d.pool || labels[labelInstance] != name {
		return machine.Instance{}, false, nil
	}
	created, _ := time.Parse(time.RFC3339Nano, img.Created)
	return committedOf(img.ID, name, labels, created), true, nil
}

// committedOf is a committed VM as its Docker image's labels describe it.
func committedOf(id, name string, labels map[string]string, created time.Time) machine.Instance {
	inst := machine.Instance{ID: id, Name: name, From: labels[labelFrom], GuestOS: machine.Linux, Created: created.UTC()}
	inst.CPUs, _ = strconv.Atoi(labels[labelCPUs])
	inst.Memory, _ = strconv.ParseUint(labels[labelMemory], 10, 64)
	return inst
}

// Instance is the pool's VM of the name: its committed Docker image, or
// else its container, or else an upgrade's new one.
func (d *Driver) Instance(ctx context.Context, name string) (machine.Instance, error) {
	if machine.ValidateName(name) != nil {
		return machine.Instance{}, fmt.Errorf("instance %q: %w", name, machine.ErrNotFound)
	}
	if inst, found, err := d.committed(ctx, name); err != nil || found {
		return inst, err
	}
	// While an upgrade is caught between its containers, the new one is
	// the VM.
	for _, ref := range []string{containerName(d.pool, name), upgradeName(d.pool, name)} {
		c, err := d.api.inspectContainer(ctx, ref)
		if isNotFound(err) || (err == nil && (!d.managed(c.Config.Labels) || c.Config.Labels[labelInstance] != name)) {
			continue
		}
		if err != nil {
			return machine.Instance{}, err
		}
		return d.instance(c), nil
	}
	return machine.Instance{}, fmt.Errorf("instance %q: %w", name, machine.ErrNotFound)
}

// managed reports whether a container is a VM of the pool's, or a
// readying base's, which is listed so one a crash leaves is removed; not
// something else of the daemon's, one from before ADR 26-10-23-417, whose
// labels name no pool, or one from before ADR 26-11-03-606, an instance
// of an image, which labelImage names: since then only a readying
// container carries it.
func (d *Driver) managed(labels map[string]string) bool {
	if labels[labelImage] != "" && !readyingName(labels[labelInstance]) {
		return false
	}
	return labels[labelInstance] != "" && labels[labelPool] == d.pool
}

// readyingName reports whether name is a readying container's,
// base-<6 hex>.
func readyingName(name string) bool {
	rest, ok := strings.CutPrefix(name, "base-")
	if !ok || len(rest) != 6 {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil
}

// Instances lists the pool's committed VMs and containers. A container
// whose name a committed VM holds is what a failed commit left, and is the
// committed VM's to remove; an upgrade's new container is listed as the VM
// only once its old one is gone.
func (d *Driver) Instances(ctx context.Context) ([]machine.Instance, error) {
	images, err := d.api.images(ctx, vmRepo)
	if err != nil {
		return nil, fmt.Errorf("docker: list images: %w", err)
	}
	var out, pending []machine.Instance
	taken := map[string]bool{}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			name, ok := strings.CutPrefix(tag, vmTag(d.pool, ""))
			if !ok || machine.ValidateName(name) != nil || img.Labels[labelCommitted] == "" ||
				img.Labels[labelPool] != d.pool || img.Labels[labelInstance] != name {
				continue
			}
			taken[name] = true
			out = append(out, committedOf(img.ID, name, img.Labels, time.Unix(img.Created, 0)))
		}
	}
	listed, err := d.api.containers(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker: list containers: %w", err)
	}
	for _, c := range listed {
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if !d.managed(c.Labels) || machine.ResourcePool(namePrefix, name) != d.pool || !strings.HasPrefix(name, machine.ResourcePrefix(namePrefix, d.pool)) {
			continue
		}
		info := containerInfo{ID: c.ID, Name: "/" + name, Image: c.ImageID}
		info.Config.Labels = c.Labels
		info.Created = time.Unix(c.Created, 0).UTC().Format(time.RFC3339Nano)
		inst := d.instance(info)
		if taken[inst.Name] {
			continue
		}
		if strings.HasSuffix(name, upgradeSuffix) {
			pending = append(pending, inst)
			continue
		}
		taken[inst.Name] = true
		out = append(out, inst)
	}
	// An upgrade's new container is the VM only once its old one is gone.
	for _, inst := range pending {
		if !taken[inst.Name] {
			taken[inst.Name] = true
			out = append(out, inst)
		}
	}
	slices.SortFunc(out, func(a, b machine.Instance) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// instance is a container as a VM: its name is the container's, less the
// pool's prefix and an upgrade's suffix, and its labels say the rest.
func (d *Driver) instance(c containerInfo) machine.Instance {
	labels := c.Config.Labels
	inst := machine.Instance{
		ID:      c.ID,
		Name:    strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(c.Name, "/"), machine.ResourcePrefix(namePrefix, d.pool)), upgradeSuffix),
		From:    labels[labelFrom],
		GuestOS: machine.Linux,
	}
	inst.Created, _ = time.Parse(time.RFC3339Nano, c.Created)
	inst.CPUs, _ = strconv.Atoi(labels[labelCPUs])
	inst.Memory, _ = strconv.ParseUint(labels[labelMemory], 10, 64)
	volumesOf(&inst, labels)
	return inst
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }

func isConflict(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusConflict
}

type containerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	ImageID string            `json:"ImageID"`
	Labels  map[string]string `json:"Labels"`
	State   string            `json:"State"`
	Created int64             `json:"Created"`
}

// containers lists every container, stopped ones too.
func (a *api) containers(ctx context.Context) ([]containerSummary, error) {
	var out []containerSummary
	return out, a.call(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}}, nil, &out)
}

type imageSummary struct {
	ID       string            `json:"Id"`
	RepoTags []string          `json:"RepoTags"`
	Labels   map[string]string `json:"Labels"`
	Created  int64             `json:"Created"`
}

// images lists the images in a repository, or every image for "".
func (a *api) images(ctx context.Context, repo string) ([]imageSummary, error) {
	query := url.Values{}
	if repo != "" {
		filters, err := json.Marshal(map[string][]string{"reference": {repo}})
		if err != nil {
			return nil, err
		}
		query.Set("filters", string(filters))
	}
	var out []imageSummary
	return out, a.call(ctx, http.MethodGet, "/images/json", query, nil, &out)
}
