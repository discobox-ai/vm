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

// Image is the image's Docker image, as its tag and its labels say.
func (d *Driver) Image(ctx context.Context, id string) (machine.Image, error) {
	img, err := d.api.inspectImage(ctx, imageRepo+":"+imageTag(d.project, id))
	if isNotFound(err) || (err == nil && !validImageID(id)) {
		return machine.Image{}, fmt.Errorf("docker: image %s: %w", id, machine.ErrNotFound)
	}
	if err != nil {
		return machine.Image{}, fmt.Errorf("docker: image %s: %w", id, err)
	}
	created, _ := time.Parse(time.RFC3339Nano, img.Created)
	return imageOf(id, img.Config.Labels, created), nil
}

// imageOf is an image as its Docker image's labels describe it. Every image
// is a linux guest's.
func imageOf(id string, labels map[string]string, created time.Time) machine.Image {
	img := machine.Image{ID: id, GuestOS: machine.Linux, Created: created}
	// An image whose labels name another inherited them from the image it
	// was committed over: it was made before images were labeled.
	if labels[labelID] == id {
		img.Comment = labels[labelComment]
	}
	return img
}

// validImageID refuses a tag of imageRepo that is not an image's whole ID:
// a tag from before ADR 26-10-23-417, `<project>--<id prefix>-<random>`, and
// an instance's readied base, `<project>--base-<name>`, are not.
func validImageID(id string) bool { return id != "" && !strings.Contains(id, "-") }

// Images lists the Docker images tagged in imageRepo for the project.
func (d *Driver) Images(ctx context.Context) ([]machine.Image, error) {
	images, err := d.api.images(ctx, imageRepo)
	if err != nil {
		return nil, fmt.Errorf("docker: list images: %w", err)
	}
	var out []machine.Image
	for _, img := range images {
		for _, tag := range img.RepoTags {
			id, ok := strings.CutPrefix(tag, imageRepo+":"+imageTag(d.project, ""))
			if ok && validImageID(id) {
				out = append(out, imageOf(id, img.Labels, time.Unix(img.Created, 0)))
			}
		}
	}
	return out, nil
}

// DeleteImage removes the image's tag, and with it the Docker image unless
// another tag holds it. The daemon keeps what another image is built on, so
// no image needs another's.
func (d *Driver) DeleteImage(ctx context.Context, id string) error {
	ref := imageRepo + ":" + imageTag(d.project, id)
	if err := d.api.removeImage(ctx, ref); err != nil {
		return fmt.Errorf("docker: remove image %s: %w", ref, err)
	}
	return nil
}

// tagPrefix starts every tag of the project: disco-vm/<project>/.
func (d *Driver) tagPrefix() string { return tagRepo + d.project + "/" }

// Tags lists the images tagged under disco-vm/<project>/, each the Docker
// image of the image its labels or its imageRepo tag name.
func (d *Driver) Tags(ctx context.Context) ([]machine.Tag, error) {
	images, err := d.api.images(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("docker: list images: %w", err)
	}
	var out []machine.Tag
	for _, img := range images {
		image := img.Labels[labelID]
		for _, tag := range img.RepoTags {
			if id, ok := strings.CutPrefix(tag, imageRepo+":"+imageTag(d.project, "")); ok && validImageID(id) {
				image = id
			}
		}
		for _, tag := range img.RepoTags {
			if ref, ok := strings.CutPrefix(tag, d.tagPrefix()); ok && image != "" {
				out = append(out, machine.Tag{Ref: ref, Image: image})
			}
		}
	}
	slices.SortFunc(out, func(a, b machine.Tag) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

// Tag tags the image's Docker image disco-vm/<project>/<ref>.
func (d *Driver) Tag(ctx context.Context, ref, image string) error {
	name, tag, _ := strings.Cut(ref, ":")
	err := d.api.tagImage(ctx, imageRepo+":"+imageTag(d.project, image), d.tagPrefix()+name, tag)
	if isNotFound(err) {
		return fmt.Errorf("docker: image %s: %w", image, machine.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("docker: tag %s: %w", ref, err)
	}
	return nil
}

// Untag removes the tag, which leaves the Docker image to its image's tag.
func (d *Driver) Untag(ctx context.Context, ref string) error {
	if err := d.api.removeImage(ctx, d.tagPrefix()+ref); err != nil {
		return fmt.Errorf("docker: untag %s: %w", ref, err)
	}
	return nil
}

// Bases is empty: the bases are every image of every registry, which no one
// lists. Base finds one by name.
func (*Driver) Bases(context.Context) ([]machine.Base, error) { return nil, nil }

// Base is ref as a base when the daemon has the image, or its registry
// answers for it. Nothing is pulled until Create. An image or a tag of the
// driver's, of any project, is never a base, nor is a Docker image ID.
func (d *Driver) Base(ctx context.Context, ref string) (machine.Base, bool, error) {
	// A project's images and tags are its own, so neither they nor a Docker
	// image ID, which could name another project's, are a base.
	if ref == "" || strings.HasPrefix(ref, imageRepo+":") || strings.HasPrefix(ref, tagRepo) ||
		strings.HasPrefix(ref, "sha256:") || strings.ToLower(ref) != ref {
		return machine.Base{}, false, nil
	}
	b := machine.Base{Name: ref, ID: ref, GuestOS: machine.Linux, Options: map[string]string{"image": ref}}
	if img, err := d.api.inspectImage(ctx, ref); err == nil {
		// The daemon takes an image ID's prefix for a name, and more than one
		// spelling of a name (docker.io/...), so what it found decides: a
		// Docker image tagged as one of the driver's, of any project, is never
		// a base. Its labels do not decide, since an image built on a
		// snapshot elsewhere inherits them, and would be a base until pulled.
		if strings.HasPrefix(img.ID, "sha256:"+ref) || slices.ContainsFunc(img.RepoTags, func(tag string) bool {
			return strings.HasPrefix(tag, imageRepo+":") || strings.HasPrefix(tag, tagRepo)
		}) {
			return machine.Base{}, false, nil
		}
		return b, true, nil
	} else if !isNotFound(err) {
		return machine.Base{}, false, err
	}
	switch err := d.api.distribution(ctx, ref); {
	case err == nil:
		return b, true, nil
	case isUnknownToRegistry(err):
		return machine.Base{}, false, nil
	default:
		// The registry could not be asked (DNS, a proxy, a timeout): say
		// so, rather than that the image does not exist.
		return machine.Base{}, false, fmt.Errorf("docker: ask the registry for %s: %w", ref, err)
	}
}

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
	images, err := d.imageIDs(ctx)
	if err != nil {
		return machine.Instance{}, err
	}
	return d.instance(c, images), nil
}

// managed reports whether a container is an instance of the project's, not an
// readying base's, something else of the daemon's, or one from before ADR
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
	images, err := d.imageIDs(ctx)
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
		out = append(out, d.instance(info, images))
	}
	slices.SortFunc(out, func(a, b machine.Instance) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// imageIDs maps each image's Docker image ID to the image's ID.
func (d *Driver) imageIDs(ctx context.Context) (map[string]string, error) {
	images, err := d.api.images(ctx, imageRepo)
	if err != nil {
		return nil, fmt.Errorf("docker: list images: %w", err)
	}
	out := map[string]string{}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if id, ok := strings.CutPrefix(tag, imageRepo+":"+imageTag(d.project, "")); ok && validImageID(id) {
				out[img.ID] = id
			}
		}
	}
	return out, nil
}

// instance is a container as an instance: its name is the container's, less
// the project's prefix, and its image the one whose Docker image it runs.
func (d *Driver) instance(c containerInfo, images map[string]string) machine.Instance {
	labels := c.Config.Labels
	inst := machine.Instance{
		ID:      c.ID,
		Name:    strings.TrimPrefix(strings.TrimPrefix(c.Name, "/"), machine.ResourcePrefix(namePrefix, d.project)),
		Image:   labels[labelImage],
		ImageID: images[c.Image],
		GuestOS: machine.Linux,
	}
	inst.Created, _ = time.Parse(time.RFC3339Nano, c.Created)
	if inst.ImageID == "" {
		inst.ImageID = labels[labelID]
	}
	if inst.Image == "" && inst.ImageID != "" {
		inst.Image = inst.ImageID[:min(12, len(inst.ImageID))]
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
