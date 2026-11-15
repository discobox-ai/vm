package hcs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/discobox-ai/vm/internal/iso9660"
	"github.com/discobox-ai/vm/internal/udf"
	"github.com/discobox-ai/vm/internal/wim"
)

// The retail ISO's files the medium boots WinPE from (ADR 26-11-14-816 §3,
// shape (a)): the UEFI boot tree, whose El Torito image boots without the
// "press any key" prompt, the boot manager that image's cdboot loads from
// the disc's root, and WinPE itself.
const (
	retailBootTree = "efi"
	noPromptImage  = "efi/microsoft/boot/efisys_noprompt.bin"
	bootManager    = "bootmgr.efi"
	bootSDI        = "boot/boot.sdi"
	bootWIM        = "sources/boot.wim"
	mediumVolume   = "DISCOVM"
	// answerFileName is where Setup looks for its answers on removable
	// media: the root.
	answerFileName = "autounattend.xml"
	// maxBootTree and maxBootDepth bound the files and directories the
	// medium walks in the boot tree, and how deep, against a hostile ISO.
	maxBootTree  = 512
	maxBootDepth = 16
)

// retailImage reads the retail ISO's editions and picks the one to install:
// its image file (install.wim, or install.esd) and the image's index.
func retailImage(iso *udf.FS, edition string) (string, wim.Image, error) {
	for _, name := range []string{"sources/install.wim", "sources/install.esd"} {
		f, err := iso.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", wim.Image{}, err
		}
		images, err := wim.Images(f)
		if err != nil {
			return "", wim.Image{}, fmt.Errorf("hcs: %s: %w", name, err)
		}
		im, err := chooseEdition(images, edition)
		if err != nil {
			return "", wim.Image{}, err
		}
		return name, im, nil
	}
	return "", wim.Image{}, errors.New("hcs: the ISO has no sources/install.wim or sources/install.esd; is it a Windows ISO?")
}

// mediumFiles are the medium's files: the retail ISO's boot files, read
// through its UDF tree, and the answer file, the agent, and the scripts.
func mediumFiles(iso *udf.FS, answerFile []byte, agent string) ([]iso9660.File, error) {
	var files []iso9660.File
	// Directories count against maxBootTree too: a hostile ISO's directory
	// can name its own ancestors under ordinary names, and hold no files.
	visited := 0
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > maxBootDepth {
			return fmt.Errorf("hcs: the ISO's %s nests deeper than %d directories", retailBootTree, maxBootDepth)
		}
		if visited++; visited+len(files) > maxBootTree {
			return fmt.Errorf("hcs: the ISO's %s holds over %d files and directories", retailBootTree, maxBootTree)
		}
		names, err := iso.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("hcs: the ISO's %s: %w", dir, err)
		}
		for _, name := range names {
			p := path.Join(dir, name)
			f, err := iso.Open(p)
			if err != nil {
				return fmt.Errorf("hcs: the ISO's %s: %w", p, err)
			}
			if f.Dir {
				if err := walk(p, depth+1); err != nil {
					return err
				}
				continue
			}
			if visited+len(files) >= maxBootTree {
				return fmt.Errorf("hcs: the ISO's %s holds over %d files and directories", retailBootTree, maxBootTree)
			}
			files = append(files, isoFile(p, f))
		}
		return nil
	}
	if err := walk(retailBootTree, 0); err != nil {
		return nil, err
	}
	for _, p := range []string{bootManager, bootSDI, bootWIM} {
		f, err := iso.Open(p)
		if err != nil {
			return nil, fmt.Errorf("hcs: the ISO's %s: %w", p, err)
		}
		files = append(files, isoFile(p, f))
	}
	if _, err := iso.Open(noPromptImage); err != nil {
		return nil, fmt.Errorf("hcs: the ISO has no %s, which boots it without a key press: %w", noPromptImage, err)
	}
	fi, err := os.Stat(agent)
	if err != nil {
		return nil, fmt.Errorf("hcs: the agent to install: %w", err)
	}
	files = append(files,
		bytesFile(answerFileName, answerFile),
		bytesFile(mediumDir+"/"+peScriptName, crlf(peScript)),
		bytesFile(mediumDir+"/"+specializeName, crlf(specializeScript)),
		iso9660.File{Path: mediumDir + "/" + agentName, Size: fi.Size(), Open: func() (io.ReadCloser, error) { return os.Open(agent) }},
	)
	return files, nil
}

// writeMedium writes the medium to w.
func writeMedium(w io.Writer, files []iso9660.File) error {
	return iso9660.Write(w, iso9660.Image{VolumeID: mediumVolume, Files: files, EFIBoot: noPromptImage})
}

func isoFile(p string, f *udf.File) iso9660.File {
	return iso9660.File{Path: p, Size: f.Size, Open: func() (io.ReadCloser, error) { return io.NopCloser(f.Reader()), nil }}
}

func bytesFile(p string, b []byte) iso9660.File {
	return iso9660.File{Path: p, Size: int64(len(b)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}
