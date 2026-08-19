package domain

import "testing"

func TestParseRPCType(t *testing.T) {
	tests := []struct {
		in      string
		want    RPCType
		wantErr bool
	}{
		{"json_rpc", RPCTypeJSONRPC, false},
		{"jsonrpc", RPCTypeJSONRPC, false},
		{"json-rpc", RPCTypeJSONRPC, false},
		{"JSON_RPC", RPCTypeJSONRPC, false},
		{"  json_rpc  ", RPCTypeJSONRPC, false},
		{"rest", RPCTypeREST, false},
		{"http", RPCTypeREST, false},
		{"comet_bft", RPCTypeCometBFT, false},
		{"cometbft", RPCTypeCometBFT, false},
		{"comet-bft", RPCTypeCometBFT, false},
		{"tendermint", RPCTypeCometBFT, false},
		{"grpc", RPCTypeGRPC, false},
		{"websocket", RPCTypeWebSocket, false},
		{"ws", RPCTypeWebSocket, false},
		{"", RPCTypeUnknown, true},
		{"nonsense", RPCTypeUnknown, true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseRPCType(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRPCType(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseRPCType(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// Stateless is what routes a type to transport.HTTP versus the phase-2 WS front,
// so the split matters: the four request/response types are stateless, and only
// WebSocket is not.
func TestRPCTypeStateless(t *testing.T) {
	tests := []struct {
		rpcType RPCType
		want    bool
	}{
		{RPCTypeJSONRPC, true},
		{RPCTypeREST, true},
		{RPCTypeCometBFT, true},
		{RPCTypeGRPC, true},
		{RPCTypeWebSocket, false},
		{RPCTypeUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.rpcType.String(), func(t *testing.T) {
			if got := tt.rpcType.Stateless(); got != tt.want {
				t.Errorf("%s.Stateless() = %v, want %v", tt.rpcType, got, tt.want)
			}
		})
	}
}

// Every String() form must parse back to the same type: config files are written
// with these names, and "call -rpc-type" echoes them in its errors.
func TestRPCTypeStringRoundTrips(t *testing.T) {
	all := []RPCType{RPCTypeJSONRPC, RPCTypeREST, RPCTypeCometBFT, RPCTypeGRPC, RPCTypeWebSocket}
	for _, rt := range all {
		t.Run(rt.String(), func(t *testing.T) {
			got, err := ParseRPCType(rt.String())
			if err != nil {
				t.Fatalf("ParseRPCType(%q): %v", rt.String(), err)
			}
			if got != rt {
				t.Errorf("round trip of %v gave %v", rt, got)
			}
		})
	}
	if RPCTypeUnknown.String() != "unknown" {
		t.Errorf("RPCTypeUnknown.String() = %q, want %q", RPCTypeUnknown.String(), "unknown")
	}
}

func TestEndpointSupportsTypeAndURL(t *testing.T) {
	ep := Endpoint{
		Supplier: "supplierA",
		URLs: map[RPCType]string{
			RPCTypeJSONRPC: "http://a/jsonrpc",
			RPCTypeREST:    "http://a/rest",
		},
	}

	if !ep.SupportsType(RPCTypeJSONRPC) {
		t.Error("SupportsType(json_rpc) = false, want true")
	}
	if ep.SupportsType(RPCTypeGRPC) {
		t.Error("SupportsType(grpc) = true, want false")
	}

	got, ok := ep.URL(RPCTypeREST)
	if !ok || got != "http://a/rest" {
		t.Errorf("URL(rest) = (%q, %v), want (%q, true)", got, ok, "http://a/rest")
	}
	if _, ok := ep.URL(RPCTypeGRPC); ok {
		t.Error("URL(grpc) reported ok for a type the endpoint does not advertise")
	}
}

// A supplier may advertise different URLs per type, which is why selection and
// the signer both key off the requested type rather than a single endpoint URL.
func TestEndpointPerTypeURLsAreDistinct(t *testing.T) {
	ep := Endpoint{
		Supplier: "supplierA",
		URLs: map[RPCType]string{
			RPCTypeJSONRPC: "http://a:8545",
			RPCTypeREST:    "http://a:1317",
		},
	}
	jsonURL, _ := ep.URL(RPCTypeJSONRPC)
	restURL, _ := ep.URL(RPCTypeREST)
	if jsonURL == restURL {
		t.Fatal("per-type URLs collapsed to the same value")
	}
}
