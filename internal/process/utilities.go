package process

import (
	"os/exec"
	"sort"
)

// ExpectedCLIUtilities is the baseline command-line environment made available
// to agents that execute Bash commands. Keep this list limited to portable,
// generally useful development utilities rather than project-specific tools.
var ExpectedCLIUtilities = []string{
	"awk",
	"bash",
	"curl",
	"find",
	"git",
	"grep",
	"jq",
	"lstat",
	"rg",
	"sed",
	"stat",
	"tar",
	"xargs",
}

// OptionalCLIUtilities are useful development commands that agents may use when
// present. Their absence is not a startup warning.
var OptionalCLIUtilities = []string{
	"bat",
	"bun",
	"cargo",
	"clang",
	"cmake",
	"composer",
	"delta",
	"deno",
	"docker",
	"dotnet",
	"fd",
	"fzf",
	"gcc",
	"gh",
	"go",
	"gradle",
	"java",
	"javac",
	"kubectl",
	"make",
	"mvn",
	"ninja",
	"nix",
	"node",
	"npm",
	"perl",
	"php",
	"pip",
	"pip3",
	"pnpm",
	"python",
	"python3",
	"ruby",
	"rustc",
	"shellcheck",
	"swift",
	"tree",
	"yarn",
}

// InspectCLIUtilities partitions ExpectedCLIUtilities according to PATH.
// Supplying nil uses exec.LookPath. Returned slices are sorted for stable
// warnings and system context.
func InspectCLIUtilities(lookPath func(string) (string, error)) (available, missing []string) {
	return inspectUtilities(ExpectedCLIUtilities, lookPath)
}

// InspectOptionalCLIUtilities returns optional commands found on PATH. Missing
// optional commands are intentionally omitted rather than reported as errors.
func InspectOptionalCLIUtilities(lookPath func(string) (string, error)) []string {
	available, _ := inspectUtilities(OptionalCLIUtilities, lookPath)
	return available
}

func inspectUtilities(utilities []string, lookPath func(string) (string, error)) (available, missing []string) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	for _, utility := range utilities {
		if _, err := lookPath(utility); err != nil {
			missing = append(missing, utility)
			continue
		}
		available = append(available, utility)
	}
	sort.Strings(available)
	sort.Strings(missing)
	return available, missing
}
