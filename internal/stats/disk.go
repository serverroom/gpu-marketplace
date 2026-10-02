package stats

import "path"

// existingDir is dir, or its nearest parent that exists ("/" at worst): a
// machine not yet set up has no storage directory, and its disk is the one
// that directory would be made on.
func existingDir(dir string, exists func(string) bool) string {
	for dir != "" && dir != "/" && !exists(dir) {
		parent := path.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if dir == "" {
		return "/"
	}
	return dir
}
