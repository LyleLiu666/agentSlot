package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentslot "github.com/LyleLiu666/agentSlot"
	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/tool"
	httptool "github.com/LyleLiu666/agentSlot/tool/http"
)

func TestHTTPRequestReturnsStructuredHTTPOutcome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body := make([]byte, request.ContentLength)
		_, _ = request.Body.Read(body)
		if request.Method != http.MethodPost || string(body) != "payload" || request.Header.Get("X-Request") != "yes" || request.Header.Get("X-Fixed") != "fixed" {
			t.Errorf("request = %s %q %#v", request.Method, body, request.Header)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusConflict)
		_, _ = response.Write([]byte(`{"state":"conflict"}`))
	}))
	defer server.Close()
	installed := newTool(t, server.URL, httptool.Config{
		AllowedMethods: []string{http.MethodGet, http.MethodPost}, AllowHTTP: true,
		FixedHeaders: map[string]string{"X-Fixed": "fixed"}, Timeout: time.Second,
		MaxRequestBytes: 64, MaxResponseBytes: 1024,
	})
	result := invoke(installed, map[string]any{
		"method": "POST", "url": server.URL + "/resource", "headers": map[string]string{"X-Request": "yes"}, "body": "payload",
	})
	if result.Status != tool.ResultSucceeded {
		t.Fatalf("result = %#v", result)
	}
	var output httptool.Output
	decode(t, result.Output, &output)
	if output.StatusCode != http.StatusConflict || output.ContentType != "application/json" || output.Body != `{"state":"conflict"}` || output.Truncated {
		t.Fatalf("output = %#v", output)
	}
}

func TestHTTPRequestRejectsUnapprovedSchemeHostAndRedirect(t *testing.T) {
	var outsideRequests atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		outsideRequests.Add(1)
	}))
	defer outside.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, outside.URL, http.StatusFound)
	}))
	defer redirect.Close()

	installed := newTool(t, redirect.URL, httptool.Config{
		AllowedMethods: []string{http.MethodGet}, AllowHTTP: true, Timeout: time.Second,
		MaxRequestBytes: 1, MaxResponseBytes: 64,
	})
	for name, arguments := range map[string]map[string]any{
		"host":     {"method": "GET", "url": outside.URL},
		"redirect": {"method": "GET", "url": redirect.URL},
	} {
		result := invoke(installed, arguments)
		if result.Status != tool.ResultFailed || result.Error == nil || result.Error.Code != "target_not_allowed" {
			t.Fatalf("%s result = %#v", name, result)
		}
	}
	if outsideRequests.Load() != 0 {
		t.Fatalf("disallowed target received %d requests", outsideRequests.Load())
	}

	httpsOnly := newTool(t, redirect.URL, httptool.Config{
		AllowedMethods: []string{http.MethodGet}, Timeout: time.Second,
		MaxRequestBytes: 1, MaxResponseBytes: 64,
	})
	result := invoke(httpsOnly, map[string]any{"method": "GET", "url": redirect.URL})
	if result.Error == nil || result.Error.Code != "target_not_allowed" {
		t.Fatalf("HTTP scheme result = %#v", result)
	}
}

func TestHTTPRedirectCannotChangeIntoAnUnapprovedMethod(t *testing.T) {
	var targetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/start" {
			http.Redirect(response, request, "/target", http.StatusFound)
			return
		}
		targetRequests.Add(1)
	}))
	defer server.Close()
	installed := newTool(t, server.URL, httptool.Config{
		AllowedMethods: []string{http.MethodPost}, AllowHTTP: true, Timeout: time.Second,
		MaxRequestBytes: 4, MaxResponseBytes: 64,
	})
	result := invoke(installed, map[string]any{"method": "POST", "url": server.URL + "/start"})
	if result.Error == nil || result.Error.Code != "method_not_allowed" {
		t.Fatalf("redirect method result = %#v", result)
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target received %d unapproved requests", targetRequests.Load())
	}
}

func TestHTTPRequestEnforcesRequestAndResponseLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = response.Write([]byte("123456"))
	}))
	defer server.Close()
	installed := newTool(t, server.URL, httptool.Config{
		AllowedMethods: []string{http.MethodPost}, AllowHTTP: true, Timeout: time.Second,
		MaxRequestBytes: 4, MaxResponseBytes: 4,
	})
	tooLarge := invoke(installed, map[string]any{"method": "POST", "url": server.URL, "body": "12345"})
	if tooLarge.Error == nil || tooLarge.Error.Code != "request_too_large" {
		t.Fatalf("large request = %#v", tooLarge)
	}
	truncated := invoke(installed, map[string]any{"method": "POST", "url": server.URL, "body": "1234"})
	var output httptool.Output
	decode(t, truncated.Output, &output)
	if truncated.Status != tool.ResultSucceeded || output.Body != "1234" || !output.Truncated {
		t.Fatalf("truncated response = %#v / %#v", truncated, output)
	}
}

func TestHTTPRequestTimeoutAndCancellationAreStructured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
			_, _ = response.Write([]byte("late"))
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	installed := newTool(t, server.URL, httptool.Config{
		AllowedMethods: []string{http.MethodGet}, AllowHTTP: true, Timeout: 20 * time.Millisecond,
		MaxRequestBytes: 1, MaxResponseBytes: 64,
	})
	result := invoke(installed, map[string]any{"method": "GET", "url": server.URL})
	if result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("timeout = %#v", result)
	}
}

func TestHTTPModuleContributesExplicitToolAndRejectsInvalidBoundary(t *testing.T) {
	if _, err := httptool.NewModule(httptool.Config{}); err == nil {
		t.Fatal("empty HTTP boundary accepted")
	}
	if _, err := httptool.New(httptool.Config{
		AllowedHosts: []string{"example.invalid"}, AllowedMethods: []string{"GE(T"},
		Timeout: time.Second, MaxRequestBytes: 1, MaxResponseBytes: 1,
	}); err == nil {
		t.Fatal("invalid HTTP method token accepted at construction")
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	module, err := httptool.NewModule(httptool.Config{
		AllowedHosts: []string{parsed.Host}, AllowedMethods: []string{http.MethodGet}, AllowHTTP: true,
		Timeout: time.Second, MaxRequestBytes: 1, MaxResponseBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	application := agentslot.NewApplication("http-tool", []agentslot.Module{module}, agentslot.RequireKey(tool.ToolSlot, httptool.Key))
	assembly, err := application.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agentslot.Lookup(assembly, tool.ToolSlot, httptool.Key); !ok {
		t.Fatal("HTTP tool was not contributed")
	}
}

func newTool(t *testing.T, allowedURL string, config httptool.Config) tool.Tool {
	t.Helper()
	parsed, err := url.Parse(allowedURL)
	if err != nil {
		t.Fatal(err)
	}
	config.AllowedHosts = []string{parsed.Host}
	installed, err := httptool.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return installed
}

func invoke(installed tool.Tool, arguments map[string]any) tool.ToolResult {
	encoded, _ := json.Marshal(arguments)
	return installed.Invoke(context.Background(), tool.ToolInvocation{
		Call:      tool.Call{ID: agent.ToolCallID("call-1"), Name: httptool.Key, Arguments: encoded},
		SessionID: "session-1", RunID: "run-1", StepID: "step-1",
	})
}

func decode(t *testing.T, data []byte, destination any) {
	t.Helper()
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

func TestHTTPRejectsMethodAndHeaderProtocolEscapes(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	installed := newTool(t, server.URL, httptool.Config{
		AllowedMethods: []string{http.MethodGet}, AllowHTTP: true, Timeout: time.Second,
		MaxRequestBytes: 1, MaxResponseBytes: 1,
	})
	for _, arguments := range []map[string]any{
		{"method": "DELETE", "url": server.URL},
		{"method": "GET", "url": server.URL, "headers": map[string]string{"Host": "elsewhere"}},
		{"method": "GET", "url": strings.Replace(server.URL, "http://", "ftp://", 1)},
	} {
		result := invoke(installed, arguments)
		if result.Status != tool.ResultFailed || result.Error == nil {
			t.Fatalf("protocol escape = %#v", result)
		}
	}
}
