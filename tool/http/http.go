// Package http provides an explicitly installed, allowlisted HTTP Tool.
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	agentslot "github.com/LyleLiu666/agentSlot"
	"github.com/LyleLiu666/agentSlot/tool"
)

const (
	Key      = "http_request"
	moduleID = "tool.builtin.http"
)

// Config fixes the network authority available to the model. Hosts include an
// explicit port when one is required (for example, api.example.com:8443).
type Config struct {
	AllowedHosts     []string
	AllowedMethods   []string
	AllowHTTP        bool
	FixedHeaders     map[string]string
	Timeout          time.Duration
	MaxRequestBytes  int
	MaxResponseBytes int
	Transport        nethttp.RoundTripper
}

// Output is the bounded HTTP response returned to the model. A non-2xx status
// is still a successfully observed HTTP outcome.
type Output struct {
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type,omitempty"`
	Body        string `json:"body"`
	Truncated   bool   `json:"truncated"`
}

type HTTP struct {
	config     Config
	methods    map[string]struct{}
	hosts      map[string]struct{}
	definition tool.Definition
	client     *nethttp.Client
}

var _ tool.Tool = (*HTTP)(nil)

// New validates and constructs one HTTP Tool. It never reads proxy or
// credential settings from the process environment.
func New(config Config) (*HTTP, error) {
	if len(config.AllowedHosts) == 0 || len(config.AllowedMethods) == 0 {
		return nil, errors.New("http tool: allowed hosts and methods are required")
	}
	if config.Timeout <= 0 || config.MaxRequestBytes <= 0 || config.MaxResponseBytes <= 0 {
		return nil, errors.New("http tool: timeout and payload limits must be positive")
	}
	hosts := make(map[string]struct{}, len(config.AllowedHosts))
	for _, host := range config.AllowedHosts {
		normalized, err := normalizeConfiguredHost(host)
		if err != nil {
			return nil, err
		}
		if _, duplicate := hosts[normalized]; duplicate {
			return nil, fmt.Errorf("http tool: duplicate allowed host %q", host)
		}
		hosts[normalized] = struct{}{}
	}
	methods := make(map[string]struct{}, len(config.AllowedMethods))
	for _, method := range config.AllowedMethods {
		if !validHTTPToken(method) || method != strings.ToUpper(method) {
			return nil, fmt.Errorf("http tool: invalid method %q", method)
		}
		if _, duplicate := methods[method]; duplicate {
			return nil, fmt.Errorf("http tool: duplicate allowed method %q", method)
		}
		methods[method] = struct{}{}
	}
	fixedHeaders, err := validateHeaders(config.FixedHeaders)
	if err != nil {
		return nil, fmt.Errorf("http tool: fixed headers: %w", err)
	}
	config.AllowedHosts = append([]string(nil), config.AllowedHosts...)
	config.AllowedMethods = append([]string(nil), config.AllowedMethods...)
	config.FixedHeaders = fixedHeaders
	transport := config.Transport
	if transport == nil {
		cloned := nethttp.DefaultTransport.(*nethttp.Transport).Clone()
		cloned.Proxy = nil
		transport = cloned
	}
	installed := &HTTP{config: config, methods: methods, hosts: hosts}
	installed.client = &nethttp.Client{
		Transport: transport,
		CheckRedirect: func(request *nethttp.Request, via []*nethttp.Request) error {
			if len(via) >= 5 {
				return errors.New("http tool: redirect limit exceeded")
			}
			if err := installed.validateTarget(request.URL); err != nil {
				return errTargetNotAllowed
			}
			if _, allowed := installed.methods[request.Method]; !allowed {
				return errMethodNotAllowed
			}
			return nil
		},
	}
	schema, err := tool.ParseInputSchema([]byte(`{"type":"object","properties":{"method":{"type":"string","minLength":1},"url":{"type":"string","minLength":1},"headers":{"type":"object","additionalProperties":{"type":"string"}},"body":{"type":"string"}},"required":["method","url"],"additionalProperties":false}`))
	if err != nil {
		return nil, err
	}
	installed.definition = tool.Definition{Name: Key, Description: "Send one bounded HTTP request to an explicitly allowed host", InputSchema: schema}
	return installed, nil
}

// NewModule contributes the Tool explicitly; standard applications do not
// gain network access merely by importing this package.
func NewModule(config Config) (agentslot.Module, error) {
	installed, err := New(config)
	if err != nil {
		return nil, err
	}
	return module{tool: installed}, nil
}

type module struct{ tool *HTTP }

func (module) ID() string { return moduleID }
func (m module) Register(reg agentslot.Registrar) error {
	return reg.Contribute(agentslot.Add(tool.ToolSlot, Key, tool.Tool(m.tool)))
}

func (h *HTTP) Definition() tool.Definition       { return h.definition }
func (*HTTP) ParallelSafety() tool.ParallelSafety { return tool.ParallelSafe }

type arguments struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

var (
	errTargetNotAllowed = errors.New("http tool: target not allowed")
	errMethodNotAllowed = errors.New("http tool: redirect method not allowed")
)

func (h *HTTP) Invoke(ctx context.Context, invocation tool.ToolInvocation) tool.ToolResult {
	if err := h.definition.InputSchema.ValidateArguments(invocation.Call.Arguments); err != nil {
		return failure(invocation, "invalid_arguments", "request arguments do not match the declared schema")
	}
	var input arguments
	if err := json.Unmarshal(invocation.Call.Arguments, &input); err != nil {
		return failure(invocation, "invalid_arguments", "request arguments are invalid")
	}
	if _, allowed := h.methods[input.Method]; !allowed {
		return failure(invocation, "method_not_allowed", "HTTP method is not allowed")
	}
	if len([]byte(input.Body)) > h.config.MaxRequestBytes {
		return failure(invocation, "request_too_large", "HTTP request body exceeds the configured limit")
	}
	headers, err := validateHeaders(input.Headers)
	if err != nil {
		return failure(invocation, "invalid_headers", "HTTP headers are invalid or reserved")
	}
	target, err := url.Parse(input.URL)
	if err != nil || h.validateTarget(target) != nil {
		return failure(invocation, "target_not_allowed", "HTTP target is not allowed")
	}
	requestContext, cancel := context.WithTimeout(ctx, h.config.Timeout)
	defer cancel()
	request, err := nethttp.NewRequestWithContext(requestContext, input.Method, target.String(), bytes.NewReader([]byte(input.Body)))
	if err != nil {
		return failure(invocation, "invalid_arguments", "HTTP request could not be constructed")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	for key, value := range h.config.FixedHeaders {
		request.Header.Set(key, value)
	}
	response, err := h.client.Do(request)
	if err != nil {
		switch {
		case errors.Is(err, errTargetNotAllowed):
			return failure(invocation, "target_not_allowed", "HTTP redirect target is not allowed")
		case errors.Is(err, errMethodNotAllowed):
			return failure(invocation, "method_not_allowed", "HTTP redirect method is not allowed")
		case errors.Is(requestContext.Err(), context.DeadlineExceeded):
			return failure(invocation, "timeout", "HTTP request exceeded its configured timeout")
		case errors.Is(requestContext.Err(), context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			return failure(invocation, "canceled", "HTTP request was canceled")
		default:
			return failure(invocation, "request_failed", "HTTP request failed")
		}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(h.config.MaxResponseBytes)+1))
	if err != nil {
		return failure(invocation, "response_failed", "HTTP response could not be read")
	}
	truncated := len(body) > h.config.MaxResponseBytes
	if truncated {
		body = body[:h.config.MaxResponseBytes]
		for len(body) > 0 && !utf8.Valid(body) {
			body = body[:len(body)-1]
		}
	}
	if !utf8.Valid(body) {
		return failure(invocation, "unsupported_content", "HTTP response is not UTF-8 text")
	}
	output, err := json.Marshal(Output{
		StatusCode: response.StatusCode, ContentType: response.Header.Get("Content-Type"),
		Body: string(body), Truncated: truncated,
	})
	if err != nil {
		return failure(invocation, "encoding_failed", "HTTP result could not be encoded")
	}
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultSucceeded, Output: output}
}

func (h *HTTP) validateTarget(target *url.URL) error {
	if target == nil || target.User != nil || target.Host == "" || target.Fragment != "" {
		return errTargetNotAllowed
	}
	if target.Scheme != "https" && !(h.config.AllowHTTP && target.Scheme == "http") {
		return errTargetNotAllowed
	}
	if _, allowed := h.hosts[strings.ToLower(target.Host)]; !allowed {
		return errTargetNotAllowed
	}
	return nil
}

func normalizeConfiguredHost(host string) (string, error) {
	if host == "" || strings.TrimSpace(host) != host {
		return "", fmt.Errorf("http tool: invalid allowed host %q", host)
	}
	parsed, err := url.Parse("//" + host)
	if err != nil || parsed.Host != host || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" {
		return "", fmt.Errorf("http tool: invalid allowed host %q", host)
	}
	return strings.ToLower(host), nil
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	return true
}

var reservedHeaders = map[string]struct{}{
	"Connection":          {},
	"Content-Length":      {},
	"Host":                {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func validateHeaders(source map[string]string) (map[string]string, error) {
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string]string, len(source))
	for _, key := range keys {
		canonical := textproto.CanonicalMIMEHeaderKey(key)
		value := source[key]
		if canonical == "" || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("invalid header")
		}
		if _, reserved := reservedHeaders[canonical]; reserved {
			return nil, errors.New("reserved header")
		}
		if _, duplicate := result[canonical]; duplicate {
			return nil, errors.New("duplicate header")
		}
		result[canonical] = value
	}
	return result, nil
}

func failure(invocation tool.ToolInvocation, code, message string) tool.ToolResult {
	return tool.ToolResult{CallID: invocation.Call.ID, Status: tool.ResultFailed, Error: &tool.StructuredError{Code: code, Message: message}}
}
