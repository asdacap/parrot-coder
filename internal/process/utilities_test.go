package process

import (
	"errors"
	"reflect"
	"testing"
)

func TestInspectCLIUtilitiesPartitionsExpectedUtilities(t *testing.T) {
	available, missing := InspectCLIUtilities(func(name string) (string, error) {
		if name == "bash" || name == "git" {
			return "/bin/" + name, nil
		}
		return "", errors.New("not found")
	})
	if want := []string{"bash", "git"}; !reflect.DeepEqual(available, want) {
		t.Fatalf("available = %#v, want %#v", available, want)
	}
	if len(missing) != len(ExpectedCLIUtilities)-len(available) {
		t.Fatalf("missing = %#v", missing)
	}
}

func TestInspectOptionalCLIUtilitiesReturnsOnlyAvailableUtilities(t *testing.T) {
	available := InspectOptionalCLIUtilities(func(name string) (string, error) {
		if name == "bat" || name == "python3" {
			return "/bin/" + name, nil
		}
		return "", errors.New("not found")
	})
	if want := []string{"bat", "python3"}; !reflect.DeepEqual(available, want) {
		t.Fatalf("available = %#v, want %#v", available, want)
	}
}
