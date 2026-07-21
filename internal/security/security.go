// Package security defines the security policy for sandboxed processes.
package security

// SecurityProfile defines the security policy for sandboxed processes. It
// controls which filesystem paths a sandboxed process may read or write.
type SecurityProfile interface {
	// IsReadOnly reports whether the profile restricts tool execution to
	// read-only operations.
	IsReadOnly() bool

	// AllowReadPaths returns file-system paths the process may read.
	AllowReadPaths() []string

	// AllowWritePaths returns file-system paths the process may write.
	AllowWritePaths() []string

	// DenyWritePaths returns paths within AllowWritePaths that must remain
	// read-only (e.g. protected metadata files).
	DenyWritePaths() []string
}
