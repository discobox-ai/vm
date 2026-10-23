package docker

import (
	"context"
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

// Layer is the layer's image, as its tag and its labels say.
func (d *Driver) Layer(ctx context.Context, id string) (machine.Layer, error) {
	img, err := d.api.inspectImage(ctx, layerRepo+":"+layerTag(d.project, id))
	if isNotFound(err) || (err == nil && !validLayerID(id)) {
		return machine.Layer{}, fmt.Errorf("docker: layer %s: %w", id, machine.ErrNotFound)
	}
	if err != nil {
		return machine.Layer{}, fmt.Errorf("docker: layer %s: %w", id, err)
	}
	created, _ := time.Parse(time.RFC3339Nano, img.Created)
	return layerOf(id, img.Config.Labels, created), nil
}

// layerOf is a layer as its image's labels describe it. Every layer is a
// linux guest's.
func layerOf(id string, labels map[string]string, created time.Time) machine.Layer {
	layer := machine.Layer{ID: id, GuestOS: machine.Linux, Created: created}
	// An image whose labels name another layer inherited them from its
	// parent: it was made before images were labeled.
	if labels[labelLayer] == id {
		layer.Parent, layer.Comment = labels[labelParent], labels[labelComment]
	}
	return layer
}

// validLayerID refuses a tag of layerRepo that is not a layer's whole ID, as
// a tag from before ADR 26-10-23-417, `<project>--<id prefix>-<random>`, is
// not.
func validLayerID(id string) bool { return id != "" && !strings.Contains(id, "-") }

// Layers lists the images tagged in layerRepo for the project.
func (d *Driver) Layers(ctx context.Context) ([]machine.Layer, error) {
	images, err := d.api.images(ctx, layerRepo)
	if err != nil {
		return nil, fmt.Errorf("docker: list images: %w", err)
	}
	var out []machine.Layer
	for _, img := range images {
		for _, tag := range img.RepoTags {
			id, ok := strings.CutPrefix(tag, layerRepo+":"+layerTag(d.project, ""))
			if ok && validLayerID(id) {
				out = append(out, layerOf(id, img.Labels, time.Unix(img.Created, 0)))
			}
		}
	}
	return out, nil
}

// DeleteLayer removes the layer's tag, and with it the image, unless another
// tag holds it.
func (d *Driver) DeleteLayer(ctx context.Context, id string) error {
	ref := layerRepo + ":" + layerTag(d.project, id)
	if err := d.api.removeImage(ctx, ref); err != nil {
		return fmt.Errorf("docker: remove image %s: %w", ref, err)
	}
	return nil
}

// tagPrefix starts every tag of the project: disco-vm/<project>/.
func (d *Driver) tagPrefix() string { return tagRepo + d.project + "/" }

// Tags lists the images tagged under disco-vm/<project>/, each the image of
// the layer its labels or its layerRepo tag name.
func (d *Driver) Tags(ctx context.Context) ([]machine.Tag, error) {
	images, err := d.api.images(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("docker: list images: %w", err)
	}
	var out []machine.Tag
	for _, img := range images {
		layer := img.Labels[labelLayer]
		for _, tag := range img.RepoTags {
			if id, ok := strings.CutPrefix(tag, layerRepo+":"+layerTag(d.project, "")); ok && validLayerID(id) {
				layer = id
			}
		}
		for _, tag := range img.RepoTags {
			if ref, ok := strings.CutPrefix(tag, d.tagPrefix()); ok && layer != "" {
				out = append(out, machine.Tag{Ref: ref, Layer: layer})
			}
		}
	}
	slices.SortFunc(out, func(a, b machine.Tag) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

// Tag tags the layer's image disco-vm/<project>/<ref>.
func (d *Driver) Tag(ctx context.Context, ref, layer string) error {
	name, tag, _ := strings.Cut(ref, ":")
	err := d.api.tagImage(ctx, layerRepo+":"+layerTag(d.project, layer), d.tagPrefix()+name, tag)
	if isNotFound(err) {
		return fmt.Errorf("docker: layer %s: %w", layer, machine.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("docker: tag %s: %w", ref, err)
	}
	return nil
}

// Untag removes the tag, which leaves the image to its layer's tag.
func (d *Driver) Untag(ctx context.Context, ref string) error {
	if err := d.api.removeImage(ctx, d.tagPrefix()+ref); err != nil {
		return fmt.Errorf("docker: untag %s: %w", ref, err)
	}
	return nil
}

// Bases is empty: a base layer is installed from an image a spec names.
func (*Driver) Bases(context.Context) ([]machine.Base, error) { return nil, nil }

// Instance is the project's container of the name.
func (d *Driver) Instance(ctx context.Context, name string) (machine.Instance, error) {
	if machine.ValidateName(name) != nil {
		return machine.Instance{}, fmt.Errorf("instance %q: %w", name, machine.ErrNotFound)
	}
	c, err := d.api.inspectContainer(ctx, containerName(d.project, name))
	if isNotFound(err) || (err == nil && !d.managed(c.Config.Labels)) {
		return machine.Instance{}, fmt.Errorf("instance %q: %w", name, machine.ErrNotFound)
	}
	if err != nil {
		return machine.Instance{}, err
	}
	layers, err := d.layerImages(ctx)
	if err != nil {
		return machine.Instance{}, err
	}
	return d.instance(c, layers), nil
}

// managed reports whether a container is an instance of the project's, not an
// install's, something else of the daemon's, or one from before ADR
// 26-10-23-417, whose labels name no project.
func (d *Driver) managed(labels map[string]string) bool {
	return labels[labelInstance] != "" && labels[labelProject] == d.project
}

// Instances lists the project's containers.
func (d *Driver) Instances(ctx context.Context) ([]machine.Instance, error) {
	listed, err := d.api.containers(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker: list containers: %w", err)
	}
	layers, err := d.layerImages(ctx)
	if err != nil {
		return nil, err
	}
	var out []machine.Instance
	for _, c := range listed {
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if !d.managed(c.Labels) || machine.ResourceProject(namePrefix, name) != d.project || !strings.HasPrefix(name, machine.ResourcePrefix(namePrefix, d.project)) {
			continue
		}
		info := containerInfo{ID: c.ID, Name: "/" + name, Image: c.ImageID}
		info.Config.Labels = c.Labels
		info.Created = time.Unix(c.Created, 0).UTC().Format(time.RFC3339Nano)
		out = append(out, d.instance(info, layers))
	}
	slices.SortFunc(out, func(a, b machine.Instance) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// layerImages maps each layer image's ID to its layer.
func (d *Driver) layerImages(ctx context.Context) (map[string]string, error) {
	images, err := d.api.images(ctx, layerRepo)
	if err != nil {
		return nil, fmt.Errorf("docker: list images: %w", err)
	}
	out := map[string]string{}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if id, ok := strings.CutPrefix(tag, layerRepo+":"+layerTag(d.project, "")); ok && validLayerID(id) {
				out[img.ID] = id
			}
		}
	}
	return out, nil
}

// instance is a container as an instance: its name is the container's, less
// the project's prefix, and its layer the one whose image it runs.
func (d *Driver) instance(c containerInfo, layers map[string]string) machine.Instance {
	labels := c.Config.Labels
	inst := machine.Instance{
		ID:      c.ID,
		Name:    strings.TrimPrefix(strings.TrimPrefix(c.Name, "/"), machine.ResourcePrefix(namePrefix, d.project)),
		Image:   labels[labelImage],
		Layer:   layers[c.Image],
		GuestOS: machine.Linux,
	}
	inst.Created, _ = time.Parse(time.RFC3339Nano, c.Created)
	if inst.Layer == "" {
		inst.Layer = labels[labelLayer]
	}
	if inst.Image == "" && inst.Layer != "" {
		inst.Image = inst.Layer[:min(12, len(inst.Layer))]
	}
	inst.CPUs, _ = strconv.Atoi(labels[labelCPUs])
	inst.Memory, _ = strconv.ParseUint(labels[labelMemory], 10, 64)
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
