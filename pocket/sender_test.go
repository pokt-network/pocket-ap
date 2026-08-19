package pocket

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pokt-network/pocket-ap/domain"
)

// TestRpcTypeHeader pins the exact wire values the relay miner routes on.
//
// These numbers are a protocol contract, not an implementation detail: the miner
// runs strconv.Atoi on the Rpc-Type header and casts the result straight to
// sharedtypes.RPCType to pick the backend service config (poktroll
// pkg/relayer/proxy/sync.go). If poktroll ever renumbers that enum, this test is
// the thing that must fail — silently stamping the old number would route relays
// to the wrong backend.
func TestRpcTypeHeader(t *testing.T) {
	tests := []struct {
		name    string
		rpcType domain.RPCType
		want    string
	}{
		{"grpc", domain.RPCTypeGRPC, "1"},
		{"websocket", domain.RPCTypeWebSocket, "2"},
		{"json_rpc", domain.RPCTypeJSONRPC, "3"},
		{"rest", domain.RPCTypeREST, "4"},
		{"comet_bft", domain.RPCTypeCometBFT, "5"},
		{"unknown stamps nothing", domain.RPCTypeUnknown, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rpcTypeHeader(tt.rpcType); got != tt.want {
				t.Errorf("rpcTypeHeader(%s) = %q, want %q", tt.rpcType, got, tt.want)
			}
		})
	}
}

// TestRpcTypeToSharedCoversAllDomainTypes guards the mapping against a new
// domain.RPCType being added without a shared-type entry, which would silently
// drop the Rpc-Type header for that type.
func TestRpcTypeToSharedCoversAllDomainTypes(t *testing.T) {
	all := []domain.RPCType{
		domain.RPCTypeJSONRPC,
		domain.RPCTypeREST,
		domain.RPCTypeCometBFT,
		domain.RPCTypeGRPC,
		domain.RPCTypeWebSocket,
	}
	for _, rt := range all {
		if _, ok := rpcTypeToShared[rt]; !ok {
			t.Errorf("domain.RPCType %s has no rpcTypeToShared entry", rt)
		}
	}
	if _, ok := rpcTypeToShared[domain.RPCTypeUnknown]; ok {
		t.Error("RPCTypeUnknown must not map to a shared type")
	}
}

// TestHTTPSender_SendStampsRpcType checks what actually reaches the wire for each
// RPC type: this is the half of REST/gRPC passthrough that does not need a live
// supplier to verify.
func TestHTTPSender_SendStampsRpcType(t *testing.T) {
	tests := []struct {
		name           string
		rpcType        domain.RPCType
		wantRpcType    string
		wantHeaderSent bool
	}{
		{"rest", domain.RPCTypeREST, "4", true},
		{"grpc", domain.RPCTypeGRPC, "1", true},
		{"comet_bft", domain.RPCTypeCometBFT, "5", true},
		{"unknown sends no header", domain.RPCTypeUnknown, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				gotMethod      string
				gotRpcType     string
				gotContentType string
				gotBody        []byte
				headerPresent  bool
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotRpcType = r.Header.Get("Rpc-Type")
				_, headerPresent = r.Header["Rpc-Type"]
				gotContentType = r.Header.Get("Content-Type")
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = w.Write([]byte("supplier-response"))
			}))
			defer srv.Close()

			relayBz := []byte("signed-relay-bytes")
			respBz, err := NewHTTPSender(5*time.Second).
				Send(context.Background(), srv.URL, relayBz, tt.rpcType)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}

			// The relay envelope always goes out as a POST regardless of the
			// inbound method; the inbound method rides inside the signed payload.
			if gotMethod != http.MethodPost {
				t.Errorf("method = %q, want POST", gotMethod)
			}
			if headerPresent != tt.wantHeaderSent {
				t.Errorf("Rpc-Type present = %v, want %v", headerPresent, tt.wantHeaderSent)
			}
			if gotRpcType != tt.wantRpcType {
				t.Errorf("Rpc-Type = %q, want %q", gotRpcType, tt.wantRpcType)
			}
			if gotContentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", gotContentType)
			}
			if string(gotBody) != string(relayBz) {
				t.Errorf("body = %q, want %q", gotBody, relayBz)
			}
			if string(respBz) != "supplier-response" {
				t.Errorf("response = %q, want %q", respBz, "supplier-response")
			}
		})
	}
}

// TestHTTPSender_SendReturnsBodyOnHTTPError documents deliberate behaviour: Send
// does not inspect the status code. A relay-miner error page comes back as
// respBz and fails later in ValidateResponse, which then fails over to the next
// supplier. Failover is correct, but the surfaced error names validation rather
// than the HTTP status — worth knowing when reading a confusing relay error.
func TestHTTPSender_SendReturnsBodyOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("miner exploded"))
	}))
	defer srv.Close()

	respBz, err := NewHTTPSender(5*time.Second).
		Send(context.Background(), srv.URL, []byte("x"), domain.RPCTypeJSONRPC)
	if err != nil {
		t.Fatalf("Send returned an error for a 500; expected the body instead: %v", err)
	}
	if string(respBz) != "miner exploded" {
		t.Errorf("respBz = %q, want the error body passed through", respBz)
	}
}

// TestHTTPSender_SendContextCancelled ensures a cancelled context aborts rather
// than hanging: the relay flow depends on this to honour its deadline.
func TestHTTPSender_SendContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewHTTPSender(5*time.Second).
		Send(ctx, srv.URL, []byte("x"), domain.RPCTypeJSONRPC); err == nil {
		t.Fatal("Send with a cancelled context returned nil error, want failure")
	}
}
