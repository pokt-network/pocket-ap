package transport

import (
	"net/http"
	"strings"
)

// gRPC returns its status in HTTP/2 TRAILERS, after the body. Shannon's
// POKTHTTPResponse has no trailer field, so the relay miner folds them into the
// response HEADERS instead (pocket-relay-miner relay_grpc_service.go:718
// mergeTrailersIntoHeader). The envelope survives, but what reaches us is not
// what a gRPC client expects.
//
// So this listener has to undo the fold: take grpc-* back out of the headers and
// emit them as real trailers. Hand a gRPC client grpc-status as a header
// alongside a body and it cannot interpret the reply — the status is required to
// arrive after the message.
//
// (The one case where a header IS correct is "Trailers-Only": an RPC that fails
// with no body at all. We do not special-case it — a trailer is accepted in both
// situations, a header is not.)
var grpcTrailerHeaders = map[string]bool{
	"grpc-status":             true,
	"grpc-message":            true,
	"grpc-status-details-bin": true,
}

// isGRPCTrailerHeader reports whether a folded header belongs in the trailers.
func isGRPCTrailerHeader(name string) bool {
	return grpcTrailerHeaders[strings.ToLower(name)]
}

// splitGRPCTrailers separates a relay response's headers into real headers and
// the grpc-* values that must become trailers.
//
// The returned trailers are written with http.TrailerPrefix, which is the
// mechanism for trailers that are not known before the headers go out — exactly
// this case, since the whole relay has to complete before we see them.
func splitGRPCTrailers(header map[string][]string) (headers, trailers map[string][]string) {
	headers = make(map[string][]string, len(header))
	trailers = map[string][]string{}
	for name, values := range header {
		if isGRPCTrailerHeader(name) {
			trailers[name] = values
			continue
		}
		headers[name] = values
	}
	return headers, trailers
}

// writeGRPCTrailers emits the split-out grpc-* values as HTTP trailers.
//
// Safe to call after WriteHeader and after the body: that is the point of
// TrailerPrefix. Go strips the prefix and sends them in the trailers once the
// handler returns.
func writeGRPCTrailers(w http.ResponseWriter, trailers map[string][]string) {
	for name, values := range trailers {
		for _, v := range values {
			w.Header().Set(http.TrailerPrefix+name, v)
		}
	}
}
