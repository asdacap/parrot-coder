package security

import "testing"

type testProfile struct {
	readOnly bool
	rules    []Rule
}

func (p testProfile) IsReadOnly() bool { return p.readOnly }
func (p testProfile) Rules() []Rule    { return p.rules }

func TestCanWriteAppliesDefaultsAndOrderedRules(t *testing.T) {
	tests := []struct {
		name    string
		profile SecurityProfile
		path    string
		want    bool
	}{
		{"nil profile", nil, "/work/file", true},
		{"read-only default", testProfile{readOnly: true}, "/work/file", false},
		{"writable default", testProfile{}, "/work/file", true},
		{"exact allow", testProfile{readOnly: true, rules: []Rule{{Path: "/work/file", Action: ActionAllowWrite}}}, "/work/file", true},
		{"directory allow", testProfile{readOnly: true, rules: []Rule{{Path: "/work", Action: ActionAllowWrite}}}, "/work/dir/file", true},
		{"component boundary", testProfile{readOnly: true, rules: []Rule{{Path: "/work/file", Action: ActionAllowWrite}}}, "/work/file.other", false},
		{"later deny", testProfile{readOnly: true, rules: []Rule{{Path: "/work", Action: ActionAllowWrite}, {Path: "/work/private", Action: ActionDenyWrite}}}, "/work/private/file", false},
		{"later allow", testProfile{readOnly: true, rules: []Rule{{Path: "/work", Action: ActionDenyWrite}, {Path: "/work/file", Action: ActionAllowWrite}}}, "/work/file", true},
		{"first reverse match decides", testProfile{readOnly: true, rules: []Rule{{Path: "/work/file", Action: ActionAllowWrite}, {Path: "/other", Action: ActionAllowWrite}, {Path: "/work", Action: ActionDenyWrite}}}, "/work/file", false},
		{"allow read revokes write", testProfile{rules: []Rule{{Path: "/work/file", Action: ActionAllowRead}}}, "/work/file", false},
		{"deny read revokes write", testProfile{rules: []Rule{{Path: "/work/file", Action: ActionDenyRead}}}, "/work/file", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CanWrite(test.profile, test.path); got != test.want {
				t.Fatalf("CanWrite(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}
