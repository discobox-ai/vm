package guest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// processEnv is the environment a guest process starts with. The agent is a
// launchd daemon, whose PATH is only the system's, so PATH is rebuilt for each
// process the way path_helper builds a login shell's: /etc/paths, then every
// file in /etc/paths.d, then whatever the agent had. A toolchain one build step
// adds there (Homebrew's /etc/paths.d/homebrew) is on PATH for the next.
func processEnv() []string {
	env := os.Environ()
	path := darwinPath("/etc", os.Getenv("PATH"))
	env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	return append(env, "PATH="+path)
}

func darwinPath(etc, current string) string {
	var dirs []string
	add := func(dir string) {
		if dir = strings.TrimSpace(dir); dir != "" && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	readLines := func(file string) {
		data, err := os.ReadFile(file)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			add(line)
		}
	}
	readLines(filepath.Join(etc, "paths"))
	if entries, err := os.ReadDir(filepath.Join(etc, "paths.d")); err == nil {
		for _, entry := range entries {
			readLines(filepath.Join(etc, "paths.d", entry.Name()))
		}
	}
	for _, dir := range filepath.SplitList(current) {
		add(dir)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}
