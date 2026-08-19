package pocket

import (
	"context"
	"fmt"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sdk "github.com/pokt-network/shannon-sdk"
	sdktypes "github.com/pokt-network/shannon-sdk/types"

	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/relay"
)

// Compile-time assertion: Validator satisfies the response Validator seam.
var _ relay.Validator = (*Validator)(nil)

// Validator verifies the supplier's RelayResponse signature and unwraps the
// inner HTTP response into a domain.RelayResult.
//
// LIFT: SAGE protocol/shannon/fullnode.go:113 ValidateRelayResponse + the
// deserialize/return block in relayer.go:237-270.
type Validator struct {
	// pubKeys is the full node's shared key cache. The SDK refetches the
	// supplier's public key on EVERY signature check, so without this each relay
	// paid a second network round trip — to the full node — for a key that never
	// changes. See cachingPubKeyFetcher.
	pubKeys sdk.PublicKeyFetcher
}

// NewValidator builds a Validator over the full node's shared key cache.
func NewValidator(fn *FullNode) *Validator {
	return &Validator{pubKeys: fn.PubKeyFetcher()}
}

// minerErrDetail renders the relay miner's own error report, when the SDK leaves
// one reachable. Empty string when there is nothing to report.
//
// WHY: the miner reports its internal failures in RelayResponse.RelayMinerError
// rather than as a transport error, so without this a failure *inside the miner*
// is indistinguishable from one at the backend — and the miner is the only thing
// that can tell you which.
//
// ⚠️ Reachable on exactly ONE branch, do not document it as always available:
// shannon-sdk returns the response alongside the error only when ValidateBasic
// fails (relay.go:99, "Even if the relay response is invalid, return it (may
// contain failure reason)"). Unmarshal (:94), pubkey fetch (:107), nil pubkey
// (:112) and signature (:116) failures all return a nil response.
func minerErrDetail(resp *servicetypes.RelayResponse) string {
	if resp == nil || resp.RelayMinerError == nil {
		return ""
	}
	e := resp.RelayMinerError
	return fmt.Sprintf(" [relay miner reported: codespace=%s code=%d %s: %s]",
		e.Codespace, e.Code, e.Description, e.Message)
}

// ValidateResponse checks the supplier signature over respBz, then deserializes
// the embedded POKTHTTPResponse into status/header/body.
func (v *Validator) ValidateResponse(supplier domain.EndpointAddr, respBz []byte) (*domain.RelayResult, error) {
	relayResp, err := sdk.ValidateRelayResponse(
		context.Background(),
		sdk.SupplierAddress(string(supplier)),
		respBz,
		v.pubKeys,
	)
	if err != nil {
		return nil, fmt.Errorf("validate relay response from %s: %w%s", supplier, err, minerErrDetail(relayResp))
	}

	poktResp, err := sdktypes.DeserializeHTTPResponse(relayResp.Payload)
	if err != nil {
		return nil, fmt.Errorf("deserialize relay response from %s: %w", supplier, err)
	}

	header := make(map[string][]string, len(poktResp.Header))
	for k, h := range poktResp.Header {
		if h == nil {
			continue
		}
		header[k] = h.Values
	}

	return &domain.RelayResult{
		StatusCode: int(poktResp.StatusCode),
		Header:     header,
		Body:       poktResp.BodyBz,
	}, nil
}
