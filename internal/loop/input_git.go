package loop

import (
	"os"
	"strings"
)

// noGitEnv removes caller Git variables and EMAIL, which Git takes for an identity, and pins tree attributes.
func noGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "EMAIL=") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_ATTR_SOURCE=4b825dc642cb6eb9a060e54bf8d69288fbee4904", "GIT_ATTR_NOSYSTEM=1")
}

// GitSafety pins execution settings for every Git call outside a sandbox. Tree attributes cannot select
// transformations, and no signature format or signing setting may run a repository's program.
func GitSafety() []string {
	return []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.attributesFile=/dev/null", "-c", "gpg.program=false", "-c", "gpg.openpgp.program=false",
		"-c", "gpg.x509.program=false", "-c", "gpg.ssh.program=false", "-c", "log.showSignature=false",
		"-c", "push.gpgSign=false",
		// a submodule is another repository, with settings of its own: no git call of momentum's fetches, pushes or
		// updates one (a status still reads its work tree)
		"-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false", "-c", "push.recurseSubmodules=no",
		// no maintenance runs on the side of a call: it runs what the repository's maintenance settings name
		"-c", "gc.auto=0", "-c", "maintenance.auto=false"}
}

// GitEnv removes caller Git settings and disables system and global settings and tree attributes.
func GitEnv() []string {
	return append(noGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat")
}
