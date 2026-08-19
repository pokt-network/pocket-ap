package websockets

import (
	"net/http"
	"testing"
)

// The origin policy is the security boundary. pocket-ap is unauthenticated on
// localhost and holds the app's private key, and WebSocket is not covered by the
// same-origin policy — so a permissive default hands every site the user visits
// the ability to relay with their key. SAGE allows all origins because it is an
// authenticated gateway; copying that here would be the bug.
func TestOriginPolicy(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		origin  string
		want    bool
	}{
		{
			name:    "no Origin header is allowed by default (native client: node, curl, go)",
			allowed: nil,
			origin:  "",
			want:    true,
		},
		{
			name:    "browser origin is rejected by default",
			allowed: nil,
			origin:  "https://evil.example.com",
			want:    false,
		},
		{
			name:    "localhost browser origin is still rejected by default",
			allowed: nil,
			origin:  "http://localhost:3000",
			want:    false,
		},
		{
			name:    "allowlisted origin is permitted",
			allowed: []string{"http://localhost:3000"},
			origin:  "http://localhost:3000",
			want:    true,
		},
		{
			name:    "origin outside the allowlist is rejected",
			allowed: []string{"http://localhost:3000"},
			origin:  "https://evil.example.com",
			want:    false,
		},
		{
			name:    "allowlist match is case-insensitive on scheme and host",
			allowed: []string{"http://LocalHost:3000"},
			origin:  "http://localhost:3000",
			want:    true,
		},
		{
			name:    "a different port is a different origin",
			allowed: []string{"http://localhost:3000"},
			origin:  "http://localhost:3001",
			want:    false,
		},
		{
			name:    "a different scheme is a different origin",
			allowed: []string{"https://localhost:3000"},
			origin:  "http://localhost:3000",
			want:    false,
		},
		{
			name:    "explicit wildcard opts everything in",
			allowed: []string{"*"},
			origin:  "https://evil.example.com",
			want:    true,
		},
		{
			name:    "wildcard among others still opts everything in",
			allowed: []string{"http://localhost:3000", "*"},
			origin:  "https://evil.example.com",
			want:    true,
		},
		{
			name:    "no Origin is allowed even with an allowlist set",
			allowed: []string{"http://localhost:3000"},
			origin:  "",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8546/", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}

			if got := (OriginPolicy{AllowedOrigins: tt.allowed}).checkOrigin(r); got != tt.want {
				t.Errorf("checkOrigin(origin=%q, allowed=%v) = %v, want %v", tt.origin, tt.allowed, got, tt.want)
			}
		})
	}
}

// The zero value is what someone gets by forgetting to configure anything, so it
// must be the safe one.
func TestOriginPolicyZeroValueRejectsBrowsers(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8546/", nil)
	r.Header.Set("Origin", "https://evil.example.com")

	var policy OriginPolicy // zero value
	if policy.checkOrigin(r) {
		t.Error("the zero-value OriginPolicy allowed a cross-origin browser connection")
	}
}
