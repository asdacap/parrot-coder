package change

import "testing"

func FuzzParsePatch(f *testing.F) {
	for _, seed := range []string{
		aiderBlock("a.txt", "", "hello"),
		aiderBlock("a.txt", "old", "new"),
		aiderBlock("a.txt", "old", "new") + aiderBlock("", "other", "changed"),
		aiderBlock("a.txt", "old", "new") + aiderBlock("b.txt", "", "created"),
		"a.txt\n" + patchSearchMarker + "\nunterminated\n",
		"not a patch",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = ParsePatch(input)
	})
}
