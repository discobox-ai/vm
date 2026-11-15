package hcs

import (
	"strings"
	"testing"

	"github.com/discobox-ai/vm/internal/wim"
)

var images = []wim.Image{
	{Index: 1, Name: "Windows 11 Home", EditionID: "Core"},
	{Index: 2, Name: "Windows 11 Education", EditionID: "Education"},
	{Index: 3, Name: "Windows 11 Pro", EditionID: "Professional"},
	{Index: 4, Name: "Windows 11 Pro N", EditionID: "ProfessionalN"},
}

func TestChooseEdition(t *testing.T) {
	for edition, want := range map[string]int{
		"":                     3, // the first Pro
		"2":                    2,
		"windows 11 pro n":     4,
		"Windows 11 Education": 2,
	} {
		im, err := chooseEdition(images, edition)
		if err != nil || im.Index != want {
			t.Errorf("chooseEdition(%q) = %d, %v; want %d", edition, im.Index, err, want)
		}
	}
	if im, _ := chooseEdition(images[:2], ""); im.Index != 1 {
		t.Errorf("with no Pro, chooseEdition picks %d, not the first", im.Index)
	}
	for _, edition := range []string{"9", "Windows 11 Enterprise", "Pro"} {
		_, err := chooseEdition(images, edition)
		if err == nil || !strings.Contains(err.Error(), "3 Windows 11 Pro") {
			t.Errorf("chooseEdition(%q): %v; want an error listing the editions", edition, err)
		}
	}
}
