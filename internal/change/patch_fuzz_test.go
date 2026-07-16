package change

import "testing"

func FuzzParsePatch(f *testing.F) {
	for _, seed := range []string{
		"*** Begin Patch\n*** Add File: a.txt\n+hello\n*** End Patch",
		"*** Begin Patch\n*** Update File: a.txt\n@@\n-old\n+new\n*** End Patch",
		"*** Begin Patch\n*** Move File: a.txt -> b.txt\n@@\n-old\n+new\n*** End Patch",
		"*** Begin Patch\n*** Delete File: a.txt\n*** End Patch",
		"not a patch",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = ParsePatch(input)
	})
}
