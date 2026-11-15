package hcs

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/discobox-ai/vm/internal/wim"
)

// chooseEdition picks the image to install (ADR 26-11-14-816 §5): edition is
// an index, or a name matched exactly without regard to case; with no
// edition, the first whose name ends in " Pro", else the first. An edition
// the file lacks is an error that lists the file's.
func chooseEdition(images []wim.Image, edition string) (wim.Image, error) {
	edition = strings.TrimSpace(edition)
	if edition == "" {
		for _, im := range images {
			if strings.HasSuffix(strings.ToLower(im.Name), " pro") {
				return im, nil
			}
		}
		return images[0], nil
	}
	if index, err := strconv.Atoi(edition); err == nil {
		for _, im := range images {
			if im.Index == index {
				return im, nil
			}
		}
	} else {
		for _, im := range images {
			if strings.EqualFold(im.Name, edition) {
				return im, nil
			}
		}
	}
	names := make([]string, len(images))
	for i, im := range images {
		names[i] = fmt.Sprintf("%d %s", im.Index, im.Name)
	}
	return wim.Image{}, fmt.Errorf("hcs: no edition %q; the image has: %s", edition, strings.Join(names, ", "))
}
