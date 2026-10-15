package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Orphans lists the daemon's containers that carry the driver's labels and
// its disco-vm-layer images, named for known's project, that known does not
// name: no instance's container.json and no layer's layer.json.
func (d *Driver) Orphans(ctx context.Context, known machine.Known) ([]machine.Orphan, error) {
	project := known.Project
	if project == "" {
		project = machine.DefaultProject
	}
	containers, images := map[string]bool{}, map[string]bool{}
	for _, inst := range known.Instances {
		var ref containerRef
		if fsutil.ReadJSON(filepath.Join(inst.Dir, containerFile), &ref) == nil {
			containers[ref.ID] = true
		}
	}
	for _, layer := range known.Layers {
		var ref layerRef
		if fsutil.ReadJSON(filepath.Join(layer.Dir, layerFile), &ref) == nil {
			images[ref.Ref] = true
		}
	}

	var out []machine.Orphan
	listed, err := d.api.containers(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker: list containers: %w", err)
	}
	for _, c := range listed {
		_, inst := c.Labels[labelInstance]
		_, layer := c.Labels[labelLayer]
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if (!inst && !layer) || machine.ResourceProject(namePrefix, name) != project || containers[c.ID] {
			continue
		}
		out = append(out, machine.Orphan{
			Kind: machine.OrphanMachine, Name: name, ID: c.ID, Status: c.State, Created: time.Unix(c.Created, 0),
		})
	}
	layers, err := d.api.images(ctx, layerRepo)
	if err != nil {
		return nil, fmt.Errorf("docker: list images: %w", err)
	}
	for _, img := range layers {
		for _, tag := range img.RepoTags {
			if machine.ResourceProject(layerRepo+":", tag) != project || images[tag] {
				continue
			}
			out = append(out, machine.Orphan{
				Kind: machine.OrphanLayer, Name: tag, ID: img.ID, Created: time.Unix(img.Created, 0),
			})
		}
	}
	slices.SortFunc(out, func(a, b machine.Orphan) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// RemoveOrphan removes an orphaned container, running or not, or an orphaned
// layer image's tag, and with it the image unless another tag holds it.
func (d *Driver) RemoveOrphan(ctx context.Context, orphan machine.Orphan) error {
	switch orphan.Kind {
	case machine.OrphanMachine:
		if err := d.api.removeContainer(ctx, orphan.ID); err != nil {
			return fmt.Errorf("docker: remove %s: %w", orphan.Name, err)
		}
		return nil
	case machine.OrphanLayer:
		if err := d.api.removeImage(ctx, orphan.Name); err != nil {
			return fmt.Errorf("docker: remove image %s: %w", orphan.Name, err)
		}
		return nil
	default:
		return fmt.Errorf("docker: orphan %s is a %q", orphan.Name, orphan.Kind)
	}
}

type containerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
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
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Created  int64    `json:"Created"`
}

// images lists the images in a repository.
func (a *api) images(ctx context.Context, repo string) ([]imageSummary, error) {
	filters, err := json.Marshal(map[string][]string{"reference": {repo}})
	if err != nil {
		return nil, err
	}
	var out []imageSummary
	return out, a.call(ctx, http.MethodGet, "/images/json", url.Values{"filters": {string(filters)}}, nil, &out)
}
