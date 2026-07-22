# Security Policy

## Reporting A Vulnerability

Report suspected vulnerabilities through a private GitHub security advisory for
this repository: open the repository's **Security** tab, choose **Advisories**,
then **Report a vulnerability**. Do not open a public issue for an unpatched
credential leak, filesystem escape, command-execution bypass, SSRF, or remote
API exposure.

Include affected versions or commits, platform, configuration, reproduction
steps, impact, and any suggested mitigation. Remove live credentials and other
people's data from reports. Maintainers will coordinate validation, remediation,
and disclosure in the advisory. No response-time or bounty commitment is made.

## Supported Versions

The project is in its initial implementation phase. Security fixes target the
latest revision and the latest published release when one exists; older builds
may require upgrading.

## Threat Boundaries

Parrot treats model/provider output, tool calls, project files and config, MCP
servers, LSP servers, formatter output, and fetched web content as untrusted.
Credentials, canonical permission binding, workspace containment, bounded I/O,
terminal sanitation, process-group cleanup, and loopback HTTP binding are in
scope for security reports.

The default `shell` tool runs in a mandatory operating-system sandbox:
Bubblewrap on Linux and Seatbelt on macOS. The host filesystem is read-only,
the workspace and Git metadata are writable except for existing Parrot metadata
 along the startup configuration path, and network access is allowed. Linked
 worktree common Git directories outside the workspace are also writable. Shell
execution fails when the sandbox is unavailable. The separate
`unrestricted_shell` tool requires permission and runs
without the sandbox, with the invoking user's local authority. Configured local
formatter, LSP, and MCP executables are likewise trusted services with that
authority. Another same-user process, a
compromised provider account, malicious dependencies used to build Parrot, and
the confidentiality of intentionally readable, fetched, or submitted content
are outside the isolation boundary. The unauthenticated HTTP API must remain on
loopback or be placed behind an independently managed authenticated proxy.
