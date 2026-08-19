package pocket

import "testing"

// TestBuildTargetURL covers path preservation — the half of REST/CometBFT
// passthrough that JSON-RPC never exercises, because JSON-RPC posts everything
// to the bare supplier URL and carries its method in the body.
//
// The inbound path arrives from two places with different guarantees:
// transport.HTTP passes r.URL.RequestURI(), which always begins with "/", while
// "call" passes -path straight from a human, which may not.
func TestBuildTargetURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		path string
		want string
	}{
		{
			name: "empty path leaves the base untouched",
			base: "https://supplier.example.com",
			path: "",
			want: "https://supplier.example.com",
		},
		{
			name: "root path leaves the base untouched",
			base: "https://supplier.example.com",
			path: "/",
			want: "https://supplier.example.com",
		},
		{
			name: "rest path is appended",
			base: "https://supplier.example.com",
			path: "/cosmos/base/tendermint/v1beta1/node_info",
			want: "https://supplier.example.com/cosmos/base/tendermint/v1beta1/node_info",
		},
		{
			name: "query string is preserved",
			base: "https://supplier.example.com",
			path: "/v1/status?verbose=true&height=42",
			want: "https://supplier.example.com/v1/status?verbose=true&height=42",
		},
		{
			name: "trailing slash on base is not doubled",
			base: "https://supplier.example.com/",
			path: "/v1/status",
			want: "https://supplier.example.com/v1/status",
		},
		{
			name: "multiple trailing slashes on base collapse",
			base: "https://supplier.example.com///",
			path: "/v1/status",
			want: "https://supplier.example.com/v1/status",
		},
		{
			name: "base path is kept and the request path appended under it",
			base: "https://supplier.example.com/api",
			path: "/v1/status",
			want: "https://supplier.example.com/api/v1/status",
		},
		{
			name: "missing leading slash is normalized, not concatenated into the host",
			base: "https://supplier.example.com",
			path: "v1/status",
			want: "https://supplier.example.com/v1/status",
		},
		{
			name: "root path on a base that carries its own path keeps that path",
			base: "https://supplier.example.com/api",
			path: "/",
			want: "https://supplier.example.com/api",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildTargetURL(tt.base, tt.path); got != tt.want {
				t.Errorf("buildTargetURL(%q, %q)\n got: %q\nwant: %q", tt.base, tt.path, got, tt.want)
			}
		})
	}
}
