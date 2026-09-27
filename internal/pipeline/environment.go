package pipeline

import (
	"os"
	"strings"
)

func bazelEnvironment() []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "PATH", "USER", "LOGNAME", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TERM",
			"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "JAVA_HOME", "SYSTEMROOT", "WINDIR", "PATHEXT":
			environment = append(environment, entry)
		}
	}
	return environment
}
