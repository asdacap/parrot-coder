package tool

// SecurityProfile defines the security policy that tools check before
// execution. Tools consult the profile to determine whether their operation
// is permitted under the current policy.
type SecurityProfile interface {
	// IsReadOnly reports whether the profile is read-only and therefore
	// restricts tool execution to tools that only observe.
	IsReadOnly() bool
}
