package pocket

import (
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	sdk "github.com/pokt-network/shannon-sdk"

	"github.com/pokt-network/pocket-ap/domain"
)

// rpcTypeMapping maps poktroll sharedtypes.RPCType to domain.RPCType.
//
// NOTE: SAGE's map (endpoint.go:65) omits GRPC — it predates gRPC service
// support. We include it: GRPC is a native Shannon RPC type and unary gRPC rides
// the stateless HTTP front.
var rpcTypeMapping = map[sharedtypes.RPCType]domain.RPCType{
	sharedtypes.RPCType_JSON_RPC:  domain.RPCTypeJSONRPC,
	sharedtypes.RPCType_REST:      domain.RPCTypeREST,
	sharedtypes.RPCType_COMET_BFT: domain.RPCTypeCometBFT,
	sharedtypes.RPCType_GRPC:      domain.RPCTypeGRPC,
	sharedtypes.RPCType_WEBSOCKET: domain.RPCTypeWebSocket,
}

// endpointsFromSession extracts one domain.Endpoint per supplier from a session,
// each carrying its per-RPC-type URLs.
//
// LIFT: SAGE protocol/shannon/endpoint.go:80 endpointsFromSession (sdk.SessionFilter
// .AllEndpoints), reshaped to []domain.Endpoint (SAGE keyed a map by "supplier-url").
func endpointsFromSession(session *sessiontypes.Session) []domain.Endpoint {
	sf := sdk.SessionFilter{Session: session}

	allEndpoints, err := sf.AllEndpoints()
	if err != nil {
		return nil
	}

	out := make([]domain.Endpoint, 0, len(allEndpoints))
	for _, supplierEndpoints := range allEndpoints {
		if len(supplierEndpoints) == 0 {
			continue
		}
		ep := domain.Endpoint{
			Supplier: domain.EndpointAddr(string(supplierEndpoints[0].Supplier())),
			URLs:     make(map[domain.RPCType]string),
		}
		for _, se := range supplierEndpoints {
			dt, ok := rpcTypeMapping[se.RPCType()]
			if !ok {
				continue
			}
			ep.URLs[dt] = se.Endpoint().Url
		}
		out = append(out, ep)
	}
	return out
}
