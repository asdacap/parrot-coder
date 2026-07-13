package formatter

import (
	"os"
	"sort"
	"strings"
)

func formatterEnvironment(extra map[string]string) []string {
	values := make(map[string]string)
	for _, name := range []string{"HOME", "LANG", "LC_ALL", "PATH", "TERM", "TMPDIR", "TZ"} {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for name, value := range extra {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) || unsafeEnvironment(name) {
			continue
		}
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}

func unsafeEnvironment(name string) bool {
	name = strings.ToUpper(name)
	return strings.HasPrefix(name, "DYLD_") || name == "LD_PRELOAD" || name == "LD_LIBRARY_PATH" ||
		name == "BASH_ENV" || name == "ENV" || name == "ZDOTDIR" || name == "PYTHONSTARTUP" ||
		name == "PYTHONPATH" || name == "PERL5OPT" || name == "RUBYOPT" || name == "NODE_OPTIONS"
}
