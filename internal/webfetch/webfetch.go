// Package webfetch provides bounded, SSRF-resistant HTTP fetching.
package webfetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type Config struct {
	AllowPrivate bool
	MaxRedirects int
	MaxBodyBytes int64
	Timeout      time.Duration
	UserAgent    string
	Resolver     *net.Resolver
}

type Service struct {
	config Config
}

type Request struct {
	URL     string
	Method  string
	Headers http.Header
	Body    []byte
}

type PermissionReview struct {
	URL    string `json:"url"`
	Method string `json:"method"`
}

type Response struct {
	FinalURL    string `json:"final_url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Text        string `json:"text"`
	Truncated   bool   `json:"truncated"`
}

func New(config Config) *Service {
	if config.MaxRedirects <= 0 {
		config.MaxRedirects = 5
	}
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = 2 << 20
	}
	if config.Timeout <= 0 {
		config.Timeout = 20 * time.Second
	}
	if config.Resolver == nil {
		config.Resolver = net.DefaultResolver
	}
	if config.UserAgent == "" {
		config.UserAgent = "parrot-coder-webfetch/1"
	}
	return &Service{config: config}
}

func NewService(config Config) *Service { return New(config) }

func Review(request Request) PermissionReview {
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" {
		method = http.MethodGet
	}
	return PermissionReview{URL: request.URL, Method: method}
}

func (s *Service) Review(request Request) PermissionReview { return Review(request) }

// Fetch resolves and pins each URL host before dialing it. Redirect targets are
// independently resolved and validated before the redirect is followed.
func (s *Service) Fetch(ctx context.Context, request Request) (Response, error) {
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !validMethod(method) {
		return Response{}, errors.New("webfetch: invalid method")
	}
	parsed, err := parseURL(request.URL)
	if err != nil {
		return Response{}, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, s.config.Timeout)
	defer cancel()

	pins := &pinnedHosts{values: make(map[string][]netip.Addr)}
	if err := s.validateAndPin(fetchCtx, parsed, pins); err != nil {
		return Response{}, err
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ResponseHeaderTimeout: s.config.Timeout,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses := pins.get(host)
			if len(addresses) == 0 {
				return nil, errors.New("webfetch: attempted to dial an unvalidated host")
			}
			var errs []error
			dialer := net.Dialer{}
			for _, ip := range addresses {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				errs = append(errs, err)
			}
			return nil, errors.Join(errs...)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) > s.config.MaxRedirects {
			return errors.New("webfetch: too many redirects")
		}
		if err := s.validateAndPin(next.Context(), next.URL, pins); err != nil {
			return err
		}
		if len(via) != 0 && !sameOrigin(via[len(via)-1].URL, next.URL) {
			next.Header.Del("Authorization")
			next.Header.Del("Proxy-Authorization")
			next.Header.Del("Cookie")
		}
		return nil
	}

	httpRequest, err := http.NewRequestWithContext(fetchCtx, method, parsed.String(), bytes.NewReader(request.Body))
	if err != nil {
		return Response{}, err
	}
	httpRequest.Header = request.Headers.Clone()
	if httpRequest.Header == nil {
		httpRequest.Header = make(http.Header)
	}
	if httpRequest.Header.Get("User-Agent") == "" {
		httpRequest.Header.Set("User-Agent", s.config.UserAgent)
	}
	httpRequest.Header.Set("Accept-Encoding", "gzip")
	response, err := client.Do(httpRequest)
	if err != nil {
		return Response{}, fmt.Errorf("webfetch: request: %w", err)
	}
	defer response.Body.Close()

	contentType, err := acceptedContentType(response.Header.Get("Content-Type"))
	if err != nil {
		return Response{}, err
	}
	reader := io.Reader(response.Body)
	encoding := strings.TrimSpace(strings.ToLower(response.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return Response{}, errors.New("webfetch: unsupported content encoding")
	}
	if encoding == "gzip" {
		gzipReader, err := gzip.NewReader(response.Body)
		if err != nil {
			return Response{}, fmt.Errorf("webfetch: gzip: %w", err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	data, truncated, err := readBounded(reader, s.config.MaxBodyBytes)
	if err != nil {
		return Response{}, fmt.Errorf("webfetch: read: %w", err)
	}
	text := strings.ToValidUTF8(string(data), "")
	if contentType == "text/html" {
		text = SanitizeHTML(text)
	} else {
		text = stripControls(text)
	}
	return Response{
		FinalURL:    response.Request.URL.String(),
		Status:      response.StatusCode,
		ContentType: contentType,
		Text:        text,
		Truncated:   truncated,
	}, nil
}

func parseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("webfetch: URL is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("webfetch: URL must be HTTP or HTTPS without user information")
	}
	if u.Fragment != "" {
		u.Fragment = ""
	}
	return u, nil
}

func validMethod(method string) bool {
	if method == "" {
		return false
	}
	for _, r := range method {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune("()<>@,;:\"/[]?={}\\", r) {
			return false
		}
	}
	return true
}

type pinnedHosts struct {
	mu     sync.RWMutex
	values map[string][]netip.Addr
}

func (p *pinnedHosts) has(host string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.values[canonicalHost(host)]) != 0
}

func (p *pinnedHosts) set(host string, addresses []netip.Addr) {
	p.mu.Lock()
	p.values[canonicalHost(host)] = append([]netip.Addr(nil), addresses...)
	p.mu.Unlock()
}

func (p *pinnedHosts) get(host string) []netip.Addr {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]netip.Addr(nil), p.values[canonicalHost(host)]...)
}

func (s *Service) validateAndPin(ctx context.Context, u *url.URL, pins *pinnedHosts) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("webfetch: redirect used a forbidden scheme")
	}
	host := canonicalHost(u.Hostname())
	if host == "" || isLocalhostName(host) && !s.config.AllowPrivate {
		return errors.New("webfetch: local or empty host is forbidden")
	}
	if pins.has(host) {
		return nil
	}
	var addresses []netip.Addr
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addresses = []netip.Addr{ip.Unmap()}
	} else {
		resolved, err := s.config.Resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return fmt.Errorf("webfetch: resolve %s: %w", host, err)
		}
		for _, ip := range resolved {
			addresses = append(addresses, ip.Unmap())
		}
	}
	allowed := addresses[:0]
	for _, ip := range addresses {
		if err := validateIP(ip, s.config.AllowPrivate); err == nil {
			allowed = append(allowed, ip)
		}
	}
	if len(allowed) == 0 {
		return errors.New("webfetch: host resolves only to forbidden addresses")
	}
	sort.Slice(allowed, func(i, j int) bool { return allowed[i].Compare(allowed[j]) < 0 })
	pins.set(host, allowed)
	return nil
}

func validateIP(ip netip.Addr, allowPrivate bool) error {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() {
		return errors.New("invalid address")
	}
	private := ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		prefixContains("100.64.0.0/10", ip) || prefixContains("192.0.0.0/24", ip)
	if private && !allowPrivate {
		return errors.New("private address")
	}
	return nil
}

func prefixContains(prefix string, ip netip.Addr) bool {
	p, err := netip.ParsePrefix(prefix)
	return err == nil && p.Contains(ip)
}

func canonicalHost(host string) string { return strings.ToLower(strings.TrimSuffix(host, ".")) }

func isLocalhostName(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func acceptedContentType(header string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", errors.New("webfetch: invalid content type")
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "text/plain", "text/html", "application/json", "text/json", "text/markdown", "text/x-markdown", "application/markdown":
		return mediaType, nil
	default:
		return "", errors.New("webfetch: unsupported content type")
	}
}

func readBounded(reader io.Reader, max int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

func stripControls(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, text)
}
