package tool

// SecurityProfile defines the security policy that tools check before
// execution. All tools are always listed, but the security profile
// determines whether a tool call is permitted to run.
type SecurityProfile interface {
	// IsReadOnly reports whether the profile is read-only and therefore
	// restricts tool execution to tools that declare themselves read-only.
	IsReadOnly() bool

	// AllowsTool reports whether a tool with the given ID and read-only
	// status is permitted by this security profile.
	AllowsTool(id string, readOnly bool) bool
}
