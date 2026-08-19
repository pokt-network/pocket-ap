package relay

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"slices"
	"strings"
	"time"

	"github.com/pokt-network/pocket-ap/domain"
)

// StreamDelimiter separates signed batches inside a streaming relay response.
//
// This is a relay-miner wire constant, not ours to choose: the miner appends it
// after every COMPLETE batch (pocket-relay-miner relayer/http_stream.go:19).
const StreamDelimiter = "||POKT_STREAM||"

// maxBatchBytes bounds a single batch. The miner caps a batch at 100KB of
// payload, but the signed protobuf around it is larger, and LLM responses in
// particular carry big chunks — the miner's own client reader uses 256KB
// (pocket-relay-miner cmd/relay/stream.go).
const maxBatchBytes = 256 * 1024

// maxResponseBytes caps a non-streaming body read through the streaming path.
// Mirrors pocket.maxResponseBytes; kept here because relay must not import
// pocket (that is the wrong way round through the seams).
const maxResponseBytes = 64 << 20 // 64 MiB

// streamingMediaTypes are the response content types the relay miner
// batch-signs instead of buffering. Kept in sync with the miner's
// isStreamingResponse (relayer/proxy.go:1974).
//
// Note the decision is not ours and not the client's: the BACKEND's
// Content-Type decides, the miner copies it onto its own reply, and we read it
// back off that reply. A client cannot ask for streaming and cannot opt out.
var streamingMediaTypes = []string{"text/event-stream", "application/x-ndjson"}

// StreamSender opens a relay whose response is consumed incrementally.
//
// Separate from Sender because Sender returns []byte, which cannot express a
// stream that is still arriving — the same shape of mismatch WebSocket had. The
// caller MUST close the returned body.
//
// Concrete impl: pocket.HTTPSender.
type StreamSender interface {
	SendStream(ctx context.Context, url string, relayReqBz []byte, rpcType domain.RPCType) (body io.ReadCloser, header map[string][]string, statusCode int, err error)
}

// isStreamingResponse reports whether a relay-miner response body holds
// delimiter-separated signed batches rather than one RelayResponse.
func isStreamingResponse(header map[string][]string) bool {
	var contentType string
	for k, v := range header {
		if strings.EqualFold(k, "Content-Type") && len(v) > 0 {
			contentType = v[0]
			break
		}
	}
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return slices.Contains(streamingMediaTypes, strings.ToLower(mediaType))
}

// RelayStream runs the relay flow, handing each validated response batch to
// onBatch as it arrives.
//
// A normal response yields exactly one batch, so this is a superset of Relay:
// the caller cannot know in advance whether a response will stream, because the
// backend decides and only says so in the response. Anything serving arbitrary
// requests has to be ready for both.
//
// Failover is allowed only until the first batch reaches onBatch. After that we
// have handed bytes to the client and are committed: silently continuing a
// half-delivered stream on a different supplier would splice two token streams
// together and the client would never know.
//
// onBatch must not retain the result body beyond the call.
func (r *Relayer) RelayStream(
	ctx context.Context,
	serviceID domain.ServiceID,
	rpcType domain.RPCType,
	in domain.RelayInput,
	onBatch func(*domain.RelayResult) error,
) error {
	if r.StreamSender == nil {
		return fmt.Errorf("relay: RelayStream needs a StreamSender")
	}

	session, err := r.Sessions.Session(ctx, serviceID)
	if err != nil {
		return fmt.Errorf("relay: session fetch failed for %s: %w", serviceID, err)
	}

	ordered, err := r.Selector.Select(ctx, serviceID, session.Endpoints, rpcType)
	if err != nil {
		return fmt.Errorf("relay: endpoint selection failed for %s/%s: %w", serviceID, rpcType, err)
	}
	if len(ordered) == 0 {
		return domain.ErrNoEndpoint
	}

	observer, _ := r.Selector.(Observer)
	observe := func(supplier domain.EndpointAddr, success bool, latency time.Duration, err error) {
		if observer == nil {
			return
		}
		observer.Observe(supplier, Outcome{
			ServiceID: serviceID, RPCType: rpcType,
			Success: success, Latency: latency, Err: err,
		})
	}

	attempts := r.MaxAttempts
	if attempts <= 0 || attempts > len(ordered) {
		attempts = len(ordered)
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		ep := ordered[i]
		url, ok := ep.URL(rpcType)
		if !ok {
			lastErr = domain.ErrNoEndpoint
			continue
		}

		relayReqBz, err := r.Signer.SignRelay(ctx, session, ep, rpcType, in)
		if err != nil {
			// Our fault, not the endpoint's: abort rather than retry, and do not
			// blame the supplier.
			return fmt.Errorf("relay: sign failed: %w", err)
		}

		start := time.Now()
		body, header, _, err := r.StreamSender.SendStream(ctx, url, relayReqBz, rpcType)
		if err != nil {
			observe(ep.Supplier, false, time.Since(start), err)
			lastErr = fmt.Errorf("relay: send to %s failed: %w", ep.Supplier, err)
			continue // nothing was delivered, so failover is safe
		}

		delivered := false
		err = r.consumeBatches(ep.Supplier, body, header, func(result *domain.RelayResult) error {
			delivered = true
			return onBatch(result)
		})
		_ = body.Close()
		latency := time.Since(start)

		if err != nil {
			observe(ep.Supplier, false, latency, err)
			if delivered {
				// Committed. The client already has part of this stream.
				return fmt.Errorf("relay: stream from %s failed after delivery: %w", ep.Supplier, err)
			}
			lastErr = fmt.Errorf("relay: stream from %s failed: %w", ep.Supplier, err)
			continue
		}

		observe(ep.Supplier, true, latency, nil)
		return nil
	}

	return fmt.Errorf("relay: all %d attempt(s) failed: %w", attempts, lastErr)
}

// consumeBatches validates and forwards every batch in a relay response.
func (r *Relayer) consumeBatches(
	supplier domain.EndpointAddr,
	body io.Reader,
	header map[string][]string,
	onBatch func(*domain.RelayResult) error,
) error {
	// Not a stream: the whole body is one RelayResponse, exactly as Relay treats
	// it. This is the common case and must stay byte-identical to it.
	if !isStreamingResponse(header) {
		// Bounded for the same reason Send bounds its read: the body comes from a
		// supplier, and SendStream hands it over still open, so nothing upstream
		// has capped it. A streaming body is bounded per batch instead, by the
		// scanner below.
		respBz, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		if int64(len(respBz)) > maxResponseBytes {
			return fmt.Errorf("response from %s exceeds %d bytes", supplier, maxResponseBytes)
		}
		result, err := r.Validator.ValidateResponse(supplier, respBz)
		if err != nil {
			return err
		}
		return onBatch(result)
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, maxBatchBytes), maxBatchBytes)

	// leftover records whether the token just returned came from the atEOF path
	// (no trailing delimiter) rather than a delimiter boundary.
	leftover := false
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.Index(data, []byte(StreamDelimiter)); i >= 0 {
			leftover = false
			return i + len(StreamDelimiter), data[:i], nil
		}
		if atEOF {
			leftover = true
			return len(data), data, nil
		}
		return 0, nil, nil // need more
	})

	// A batch followed by a delimiter is complete. A trailing token without one
	// is complete only if the stream ended cleanly — if the read failed
	// mid-write it is a truncated protobuf, and validating it would fail and
	// take down a stream the client has already partly consumed. So hold the
	// last unterminated token back until we know how the stream ended.
	var pending []byte
	var pendingLeftover bool

	flush := func() error {
		if pending == nil {
			return nil
		}
		result, err := r.Validator.ValidateResponse(supplier, pending)
		if err != nil {
			return err
		}
		pending = nil
		return onBatch(result)
	}

	for scanner.Scan() {
		if err := flush(); err != nil {
			return err
		}
		batch := scanner.Bytes()
		if len(batch) == 0 {
			continue
		}
		// Copy: the scanner reuses its buffer.
		pending = append([]byte(nil), batch...)
		pendingLeftover = leftover
	}

	if err := scanner.Err(); err != nil {
		// The stream broke. Anything already delivered stays delivered; drop only
		// a trailing token we never saw terminated.
		if pending != nil && !pendingLeftover {
			if flushErr := flush(); flushErr != nil {
				return flushErr
			}
		}
		return fmt.Errorf("read stream: %w", err)
	}

	// Clean EOF: a trailing token is a complete final batch.
	return flush()
}
