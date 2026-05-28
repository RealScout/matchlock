package policy

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEngine_IsHostAllowed_NoRestrictions(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{})

	assert.True(t, engine.IsHostAllowed("example.com"), "All hosts should be allowed when no restrictions")
}

func TestEngine_IsHostAllowed_Allowlist(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.openai.com", "*.anthropic.com"},
	})

	tests := []struct {
		host    string
		allowed bool
	}{
		{"api.openai.com", true},
		{"api.anthropic.com", true},
		{"console.anthropic.com", true},
		{"evil.com", false},
		{"openai.com.evil.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.allowed, engine.IsHostAllowed(tt.host))
		})
	}
}

func TestEngine_AddAllowedHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.openai.com"},
	})

	added := engine.AddAllowedHosts(" api.openai.com ", "api.anthropic.com", "", "api.anthropic.com")
	assert.Equal(t, []string{"api.anthropic.com"}, added)
	assert.Equal(t, []string{"api.openai.com", "api.anthropic.com"}, engine.AllowedHosts())
}

func TestEngine_RemoveAllowedHosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.openai.com", "api.anthropic.com", "api.openai.com"},
	})

	removed := engine.RemoveAllowedHosts(" api.openai.com ", "missing.example.com")
	assert.Equal(t, []string{"api.openai.com"}, removed)
	assert.Equal(t, []string{"api.anthropic.com"}, engine.AllowedHosts())
}

func TestEngine_IsHostAllowed_BlockPrivateIPs(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
	})

	tests := []struct {
		host    string
		allowed bool
	}{
		{"192.168.1.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"127.0.0.1", false},
		{"8.8.8.8", true},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.allowed, engine.IsHostAllowed(tt.host))
		})
	}
}

func TestEngine_IsHostAllowed_WithPort(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"api.example.com"},
	})

	assert.True(t, engine.IsHostAllowed("api.example.com:443"), "Should allow host with port")
}

func TestEngine_GetPlaceholder(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {Value: "sk-secret-123"},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	assert.NotEmpty(t, placeholder)
	assert.True(t, strings.HasPrefix(placeholder, "SANDBOX_SECRET_"), "Placeholder should have correct prefix")
}

func TestEngine_GetPlaceholders(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"KEY1": {Value: "val1"},
			"KEY2": {Value: "val2"},
		},
	})

	placeholders := engine.GetPlaceholders()
	assert.Len(t, placeholders, 2)
}

func TestEngine_OnRequest_SecretReplacement(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	assert.Equal(t, "Bearer real-secret", result.Header.Get("Authorization"))
}

func TestEngine_OnRequest_SecretReplacementInBasicAuth(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	basic := base64.StdEncoding.EncodeToString([]byte("user:" + placeholder))

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Basic " + basic},
		},
		URL: &url.URL{},
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	auth := result.Header.Get("Authorization")
	require.True(t, strings.HasPrefix(auth, "Basic "))
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	require.NoError(t, err)
	assert.Equal(t, "user:real-secret", string(decoded))
}

func TestEngine_OnRequest_SecretReplacementInUnpaddedBasicAuth(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	basic := base64.RawStdEncoding.EncodeToString([]byte("user:" + placeholder))

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Basic " + basic},
		},
		URL: &url.URL{},
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	auth := result.Header.Get("Authorization")
	require.True(t, strings.HasPrefix(auth, "Basic "))
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	require.NoError(t, err)
	assert.Equal(t, "user:real-secret", string(decoded))
}

func TestEngine_OnRequest_SecretLeak(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Bearer " + placeholder},
		},
		URL: &url.URL{},
	}

	_, err := engine.OnRequest(req, "evil.com")
	require.ErrorIs(t, err, api.ErrSecretLeak, "Should detect secret leak to unauthorized host")
}

func TestEngine_OnRequest_SecretLeakInBasicAuth(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	basic := base64.StdEncoding.EncodeToString([]byte("user:" + placeholder))

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Basic " + basic},
		},
		URL: &url.URL{},
	}

	_, err := engine.OnRequest(req, "evil.com")
	require.ErrorIs(t, err, api.ErrSecretLeak, "Should detect basic auth secret leak to unauthorized host")
}

func TestEngine_OnRequest_SecretLeakInUnpaddedBasicAuth(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	basic := base64.RawStdEncoding.EncodeToString([]byte("user:" + placeholder))

	req := &http.Request{
		Header: http.Header{
			"Authorization": []string{"Basic " + basic},
		},
		URL: &url.URL{},
	}

	_, err := engine.OnRequest(req, "evil.com")
	require.ErrorIs(t, err, api.ErrSecretLeak, "Should detect basic auth secret leak to unauthorized host")
}

func TestEngine_OnRequest_NoSecretForHost(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	req := &http.Request{
		Header: http.Header{
			"X-Custom": []string{"normal-value"},
		},
		URL: &url.URL{},
	}

	result, err := engine.OnRequest(req, "other.com")
	require.NoError(t, err)

	assert.Equal(t, "normal-value", result.Header.Get("X-Custom"), "Non-secret values should be unchanged")
}

func TestEngine_OnRequest_SecretInURL(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")

	req := &http.Request{
		Header: http.Header{},
		URL: &url.URL{
			RawQuery: "key=" + placeholder,
		},
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	assert.Contains(t, result.URL.RawQuery, "real-secret", "Secret should be replaced in URL")
}

func TestEngine_OnRequest_NoBodyReplacement(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"api.example.com"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")
	body := `{"key":"` + placeholder + `"}`

	req := &http.Request{
		Header: http.Header{},
		URL:    &url.URL{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}

	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)

	got, _ := io.ReadAll(result.Body)
	assert.NotContains(t, string(got), "real-secret", "Secret should NOT be replaced in request body")
	assert.Contains(t, string(got), placeholder, "Placeholder should remain in request body")
}

func TestEngine_OnResponse(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{})

	resp := &http.Response{
		StatusCode: 200,
	}

	result, err := engine.OnResponse(resp, nil, "example.com")
	require.NoError(t, err)

	assert.Equal(t, resp, result, "Response should be unchanged")
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		str     string
		match   bool
	}{
		{"*", "anything", true},
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "a.b.example.com", true}, // deep subdomains
		{"*.example.com", "example.com", false},
		{"api.example.com", "api.example.com", true},
		{"api.example.com", "other.example.com", false},
		{"prefix.*", "prefix.com", true},
		{"prefix.*", "other.com", false},
		// Middle wildcard patterns
		{"api-*.example.com", "api-v1.example.com", true},
		{"api-*.example.com", "api-prod.example.com", true},
		{"api-*.example.com", "other.example.com", false},
		{"*-prod.example.com", "api-prod.example.com", true},
		{"*-prod.example.com", "api-dev.example.com", false},
		// Multiple wildcards
		{"*.*.example.com", "a.b.example.com", true},
		{"api-*-*.example.com", "api-v1-prod.example.com", true},
		// Edge cases
		{"", "", true},
		{"test", "test", true},
		{"test", "other", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.str, func(t *testing.T) {
			assert.Equal(t, tt.match, matchGlob(tt.pattern, tt.str))
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		host    string
		private bool
	}{
		{"192.168.1.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"127.0.0.1", true},
		{"169.254.1.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"172.32.0.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			assert.Equal(t, tt.private, isPrivateIP(tt.host))
		})
	}
}

// writeTokenFile writes a value to a temp file and returns its path. Used by
// ValueFile tests to simulate a host refresher writing the current secret.
func writeTokenFile(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func TestEngine_OnRequest_ValueFileSecret(t *testing.T) {
	dir := t.TempDir()
	tokenPath := writeTokenFile(t, dir, "fresh-token-1\n")

	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Placeholder: "PLACEHOLDER_TOKEN",
				ValueFile:   tokenPath,
				Hosts:       []string{"api.example.com"},
			},
		},
	})

	req := &http.Request{
		Header: http.Header{"Authorization": []string{"Bearer PLACEHOLDER_TOKEN"}},
		URL:    &url.URL{},
	}
	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer fresh-token-1", result.Header.Get("Authorization"),
		"placeholder should be replaced with the file's current contents (whitespace trimmed)")
}

func TestEngine_OnRequest_ValueFileServesStaleOnReadError(t *testing.T) {
	dir := t.TempDir()
	tokenPath := writeTokenFile(t, dir, "first-token")

	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Placeholder: "PH",
				ValueFile:   tokenPath,
				Hosts:       []string{"api.example.com"},
			},
		},
	})

	// Prime the cache with a successful read.
	req1 := &http.Request{
		Header: http.Header{"Authorization": []string{"Bearer PH"}},
		URL:    &url.URL{},
	}
	_, err := engine.OnRequest(req1, "api.example.com")
	require.NoError(t, err)

	// Force-expire the cache so the next call re-reads the file, then remove
	// the file to simulate a transient unavailability (e.g. atomic-rename mid-flight).
	engine.secretValMu.Lock()
	ent := engine.secretValCache["API_KEY"]
	ent.fetchedAt = ent.fetchedAt.Add(-2 * secretValueTTL)
	engine.secretValCache["API_KEY"] = ent
	engine.secretValMu.Unlock()
	require.NoError(t, os.Remove(tokenPath))

	req2 := &http.Request{
		Header: http.Header{"Authorization": []string{"Bearer PH"}},
		URL:    &url.URL{},
	}
	result, err := engine.OnRequest(req2, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer first-token", result.Header.Get("Authorization"),
		"a missing ValueFile should serve the last-known value rather than mangling the request")
}

func TestEngine_OnRequest_ValueFileMissingSkipsSwap(t *testing.T) {
	// No cache primed and the file doesn't exist: the swap must be skipped so
	// the placeholder passes through (caller gets a 401), not substituted with "".
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Placeholder: "PH",
				ValueFile:   filepath.Join(t.TempDir(), "does-not-exist"),
				Hosts:       []string{"api.example.com"},
			},
		},
	})

	req := &http.Request{
		Header: http.Header{"Authorization": []string{"Bearer PH"}},
		URL:    &url.URL{},
	}
	result, err := engine.OnRequest(req, "api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "Bearer PH", result.Header.Get("Authorization"),
		"with no usable value the placeholder must pass through untouched")
}
