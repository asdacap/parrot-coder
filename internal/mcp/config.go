// Package mcp implements bounded clients for Model Context Protocol servers.
package mcp

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"

	defaultStartupTimeout = 15 * time.Second
	defaultCallTimeout    = 30 * time.Second
	defaultMessageBytes   = int64(4 << 20)
	defaultOutputBytes    = int64(1 << 20)
	defaultListPages      = 100
	defaultListItems      = 10_000
	maxHeaderCount        = 64
	maxHeaderBytes        = 32 << 10
)

// Config describes one MCP server. HTTP without TLS is allowed only when both
// AllowInsecureLocalhost is true and URL names a loopback host.
type Config struct {
	Name      string            `json:"name"`
	Transport Transport         `json:"transport"`
	Enabled   bool              `json:"enabled"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`

	AllowInsecureLocalhost bool          `json:"allow_insecure_localhost,omitempty"`
	StartupTimeout         time.Duration `json:"startup_timeout,omitempty"`
	CallTimeout            time.Duration `json:"call_timeout,omitempty"`
	MaxMessageBytes        int64         `json:"max_message_bytes,omitempty"`
	MaxOutputBytes         int64         `json:"max_output_bytes,omitempty"`
	MaxListPages           int           `json:"max_list_pages,omitempty"`
	MaxListItems           int           `json:"max_list_items,omitempty"`
}

// Validate checks the configuration without starting a server.
func (c Config) Validate() error {
	_, err := c.validated()
	return err
}

func (c Config) validated() (Config, error) {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 128 {
		return Config{}, errors.New("mcp: server name must contain 1 to 128 characters")
	}
	if strings.IndexByte(c.Name, 0) >= 0 {
		return Config{}, errors.New("mcp: server name contains NUL")
	}
	for _, character := range c.Name {
		if character < 0x20 || character == 0x7f {
			return Config{}, errors.New("mcp: server name contains a control character")
		}
	}
	if c.StartupTimeout < 0 || c.CallTimeout < 0 {
		return Config{}, errors.New("mcp: timeouts cannot be negative")
	}
	if c.StartupTimeout == 0 {
		c.StartupTimeout = defaultStartupTimeout
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = defaultCallTimeout
	}
	if c.StartupTimeout > 5*time.Minute || c.CallTimeout > 30*time.Minute {
		return Config{}, errors.New("mcp: timeout exceeds configured safety limit")
	}
	if c.MaxMessageBytes < 0 || c.MaxOutputBytes < 0 || c.MaxListPages < 0 || c.MaxListItems < 0 {
		return Config{}, errors.New("mcp: limits cannot be negative")
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = defaultMessageBytes
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = defaultOutputBytes
	}
	if c.MaxMessageBytes > 64<<20 || c.MaxOutputBytes > 16<<20 {
		return Config{}, errors.New("mcp: output limit exceeds safety maximum")
	}
	if c.MaxOutputBytes > c.MaxMessageBytes {
		return Config{}, errors.New("mcp: max output bytes cannot exceed max message bytes")
	}
	if c.MaxListPages == 0 {
		c.MaxListPages = defaultListPages
	}
	if c.MaxListItems == 0 {
		c.MaxListItems = defaultListItems
	}
	if c.MaxListPages > 1_000 || c.MaxListItems > 100_000 {
		return Config{}, errors.New("mcp: list limit exceeds safety maximum")
	}

	switch c.Transport {
	case TransportStdio:
		if err := validateStdio(c); err != nil {
			return Config{}, err
		}
		if c.URL != "" || len(c.Headers) != 0 {
			return Config{}, errors.New("mcp: stdio config cannot contain HTTP fields")
		}
	case TransportHTTP:
		if err := validateHTTP(c); err != nil {
			return Config{}, err
		}
		if c.Command != "" || len(c.Args) != 0 || len(c.Env) != 0 || c.Cwd != "" {
			return Config{}, errors.New("mcp: HTTP config cannot contain stdio fields")
		}
	default:
		return Config{}, fmt.Errorf("mcp: unsupported transport %q", c.Transport)
	}
	return c, nil
}

func validateStdio(c Config) error {
	if c.Command == "" || !filepath.IsAbs(c.Command) {
		return errors.New("mcp: stdio command must be an absolute executable path")
	}
	info, err := os.Stat(c.Command)
	if err != nil {
		return fmt.Errorf("mcp: stat stdio command: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("mcp: stdio command is not executable")
	}
	if c.Cwd != "" {
		if !filepath.IsAbs(c.Cwd) {
			return errors.New("mcp: stdio cwd must be absolute")
		}
		info, err := os.Stat(c.Cwd)
		if err != nil || !info.IsDir() {
			return errors.New("mcp: stdio cwd must be an existing directory")
		}
	}
	if len(c.Args) > 256 || len(c.Env) > 128 {
		return errors.New("mcp: too many stdio arguments or environment variables")
	}
	for _, arg := range c.Args {
		if strings.IndexByte(arg, 0) >= 0 || len(arg) > 32<<10 {
			return errors.New("mcp: invalid stdio argument")
		}
	}
	for name, value := range c.Env {
		if !validEnvName(name) || strings.IndexByte(value, 0) >= 0 || len(value) > 32<<10 {
			return fmt.Errorf("mcp: invalid environment variable %q", name)
		}
		if unsafeEnvironmentName(name) {
			return fmt.Errorf("mcp: unsafe environment variable %q is not allowed", name)
		}
	}
	return nil
}

func validateHTTP(c Config) error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return errors.New("mcp: HTTP URL must be absolute")
	}
	if u.User != nil {
		return errors.New("mcp: embedded URL credentials are not allowed")
	}
	if u.Fragment != "" {
		return errors.New("mcp: HTTP URL fragments are not allowed")
	}
	if u.Scheme != "https" {
		if u.Scheme != "http" || !c.AllowInsecureLocalhost || !loopbackHost(u.Hostname()) {
			return errors.New("mcp: HTTP URL requires HTTPS (explicit loopback HTTP opt-in is available)")
		}
	}
	if len(c.Headers) > maxHeaderCount {
		return errors.New("mcp: too many HTTP headers")
	}
	total := 0
	for name, value := range c.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" || !validHeaderName(name) || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("mcp: invalid HTTP header %q", name)
		}
		switch canonical {
		case "Host", "Content-Length", "Connection", "Transfer-Encoding", "Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version":
			return fmt.Errorf("mcp: controlled HTTP header %q is not allowed", name)
		}
		total += len(name) + len(value)
	}
	if total > maxHeaderBytes {
		return errors.New("mcp: HTTP headers exceed byte limit")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

func validEnvName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func unsafeEnvironmentName(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "DYLD_") || upper == "LD_PRELOAD" || upper == "LD_LIBRARY_PATH" {
		return true
	}
	switch upper {
	case "BASH_ENV", "ENV", "ZDOTDIR", "PYTHONSTARTUP", "PYTHONPATH", "PERL5OPT", "RUBYOPT", "NODE_OPTIONS", "PROMPT_COMMAND", "CDPATH":
		return true
	default:
		return false
	}
}

func controlledEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, name := range []string{"HOME", "LANG", "LC_ALL", "PATH", "TERM", "TMPDIR", "TZ"} {
		if value, ok := os.LookupEnv(name); ok && !unsafeEnvironmentName(name) {
			values[name] = value
		}
	}
	for name, value := range overrides {
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
