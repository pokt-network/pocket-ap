package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pokt-network/pocket-ap/config"
	"github.com/pokt-network/pocket-ap/domain"
	"github.com/pokt-network/pocket-ap/internal/safego"
	"github.com/pokt-network/pocket-ap/pocket"
	"github.com/pokt-network/pocket-ap/relay"
	"github.com/pokt-network/pocket-ap/selector"
	"github.com/pokt-network/pocket-ap/transport"
)

const callUsage = `pocket-ap call — send one relay and print the response

The response body goes to stdout verbatim (pipe it to jq); diagnostics go to
stderr. This is a debug and verification tool: every invocation fetches a fresh
session from the full node, so it is not a path for production traffic.

examples:
  pocket-ap call -config local/config.yaml \
      -d '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}'

  pocket-ap call -config local/config.yaml -service pnf-anvil -rpc-type rest \
      -X GET -path /v1/status -v

  # send the same request both ways and diff the answers
  pocket-ap call -config local/config.yaml -compare https://rpc.ankr.com/eth \
      -d '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}'

  # pin this one relay to a supplier (the per-request form of suppliers.allow;
  # it can only narrow what the config allows, never widen it)
  pocket-ap call -config local/config.yaml -v \
      -H 'X-Pocket-Allow-Suppliers: pokt1…' \
      -d '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}'

flags:
`

// headerFlag collects repeatable -H "Name: value" flags into relay headers.
type headerFlag map[string][]string

func (h headerFlag) String() string { return "" }

func (h headerFlag) Set(v string) error {
	name, value, ok := strings.Cut(v, ":")
	if !ok {
		return fmt.Errorf("header %q must be in \"Name: value\" form", v)
	}
	if name = strings.TrimSpace(name); name == "" {
		return fmt.Errorf("header %q has an empty name", v)
	}
	h[name] = append(h[name], strings.TrimSpace(value))
	return nil
}

func runCall(args []string) error {
	fs := flag.NewFlagSet("call", flag.ExitOnError)
	var (
		configPath = fs.String("config", "", "path to config YAML (or set POCKET_AP_CONFIG)")
		service    = fs.String("service", "", "service ID (default: inferred from the config's listeners)")
		rpcTypeStr = fs.String("rpc-type", "", "json_rpc|rest|comet_bft|grpc (default: inferred from the config's listeners)")
		path       = fs.String("path", "/", "request path and query, relative to the supplier's URL")
		method     = fs.String("method", "", "HTTP method (default: POST with a body, GET without)")
		data       = fs.String("data", "", `request body; "@file" reads a file, "-" reads stdin`)
		timeout    = fs.Duration("timeout", 30*time.Second, "deadline for the whole call, failover included")
		verbose    = fs.Bool("v", false, "print relay diagnostics to stderr")
		compareURL = fs.String("compare", "", "also send the request straight to this URL and diff the two responses")
		network    = fs.String("network", "", "point the full node at a named network ("+strings.Join(config.NetworkNames(), "|")+"), overriding the config")
	)
	headers := headerFlag{}
	fs.Var(headers, "H", `extra request header, "Name: value" (repeatable)`)
	fs.StringVar(method, "X", "", "alias for -method")
	fs.StringVar(data, "d", "", "alias for -data")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, callUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Set the default logger before constructing anything: SessionManager
	// captures slog.Default() at construction. Quiet unless -v.
	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load(orEnv(*configPath, "POCKET_AP_CONFIG"))
	if err != nil {
		return err
	}
	if err := applyNetworkFlag(cfg, *network); err != nil {
		return err
	}
	body, err := readBody(*data)
	if err != nil {
		return fmt.Errorf("call: read body: %w", err)
	}

	fn, signers, signer, err := buildCore(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Resolving apps is what makes -service optional: each key names its own
	// service onchain. It is inside the call timeout because it is a network
	// call like any other here.
	apps, err := resolveApps(ctx, cfg, signers, slog.Default())
	if err != nil {
		return err
	}
	// Same fill-in as serve, so -rpc-type can still be inferred from listeners
	// that leave service_id out.
	if err := resolveListenerServices(cfg, apps); err != nil {
		return err
	}
	serviceID, rpcType, err := resolveTarget(cfg, apps, *service, *rpcTypeStr)
	if err != nil {
		return err
	}

	// Deliberately no SessionManager.Start: a one-shot process always misses the
	// session cache, and Session() only consults the polled block height on a
	// cache hit. The poller would cost a CometBFT round trip and change nothing.
	sessions, err := pocket.NewSessionManager(fn, serviceApps(apps))
	if err != nil {
		return err
	}

	// The Relayer reports only the final response, so which supplier answered and
	// how many failovers it took are invisible from outside. recSelector recovers
	// that by implementing relay.Observer — the same hook a QoS selector would use
	// — and recSessions adds the session. Neither touches the relay core.
	rec := &recorder{}
	grpcSender := pocket.NewGRPCSenderMode(cfg.GRPCMode)
	defer func() { _ = grpcSender.Close() }()
	sender := pocket.NewMultiSender(pocket.NewHTTPSender(*timeout), grpcSender)
	relayer := &relay.Relayer{
		Sessions:  recSessions{inner: sessions, rec: rec},
		Signer:    signer,
		Validator: pocket.NewValidator(fn),
		// Same composition order as serve, for the same reason: recSelector is
		// the Observer here, so the supplier Filter has to sit under it.
		Selector: recSelector{
			inner: selector.Filter{Inner: selector.Random{}, Policies: supplierPolicies(apps)},
			rec:   rec,
		},
		Sender: sender,
		// call streams like serve does. It must: this is the tool you reach for to
		// debug a service, and an inference backend answers with SSE — which
		// arrives as several signed batches that Relay would hand the validator as
		// one blob and fail on.
		StreamSender: sender,
		MaxAttempts:  defaultMaxAttempts,
	}

	in := domain.RelayInput{
		Method: resolveMethod(*method, body),
		Path:   *path,
		Header: headers,
		Body:   body,
	}

	// The same per-request supplier headers the serve listeners honour, so `call`
	// can reproduce — and `call -v` can show — the routing an external QoS process
	// would ask for. Taken OUT of in.Header for the same reason as there: what is
	// left gets signed and replayed to the backend. Doing it before -compare fires
	// also keeps the baseline fair, since neither side then carries the header.
	suppliers, err := transport.TakeSupplierPolicy(in.Header)
	if err != nil {
		return fmt.Errorf("call: %w", err)
	}
	ctx = domain.WithSupplierPolicy(ctx, suppliers)

	// The baseline runs concurrently with the relay: they are independent network
	// calls, and timing them separately keeps the latency comparison meaningful.
	var (
		wg     sync.WaitGroup
		direct directResult
	)
	if *compareURL != "" {
		wg.Add(1)
		go func() {
			// The baseline is a diagnostic, not the relay. A panic in it must not
			// take down a run whose actual job — the relay to stdout — is fine;
			// direct stays zero-valued and the comparison reports as failed.
			defer safego.Recover(nil, "call.compare.direct")
			defer wg.Done()
			direct = sendDirect(ctx, *compareURL, in)
		}()
	}

	// -compare has to diff a whole body against a whole body, so it buffers.
	// Without it, batches go straight out and nothing is held.
	buffering := *compareURL != ""

	var (
		first    *domain.RelayResult
		batches  int
		fullBody bytes.Buffer
	)

	start := time.Now()
	err = relayer.RelayStream(ctx, serviceID, rpcType, in, func(result *domain.RelayResult) error {
		batches++
		if first == nil {
			first = result
		}
		if buffering {
			fullBody.Write(result.Body)
		}
		// Verbatim, and immediately: the payload is opaque bytes (no trailing
		// newline is added), and os.Stdout is unbuffered, so a token stream reaches
		// the terminal as it arrives rather than in one lump at the end.
		_, writeErr := os.Stdout.Write(result.Body)
		return writeErr
	})
	elapsed := time.Since(start)
	wg.Wait()

	// Diagnostics after the body: a stream has no "before". They go to stderr, so
	// piping stdout to jq is unaffected.
	if *verbose {
		rec.dump(os.Stderr, serviceID, rpcType, elapsed)
		if batches > 1 {
			fmt.Fprintf(os.Stderr, "batches:   %d (streamed)\n", batches)
		}
	}
	if *compareURL != "" {
		compared := first
		if compared != nil {
			// Diff the whole stream, not just its first batch.
			joined := *compared
			joined.Body = fullBody.Bytes()
			compared = &joined
		}
		dumpCompare(os.Stderr, rec, *compareURL, compared, err, direct)
	}
	if err != nil {
		return err
	}

	// A non-2xx from the backend is still a completed relay, so this is a note on
	// stderr, not a failure exit. The status belongs to the first batch: HTTP
	// sends it once, before any body.
	if first != nil && first.StatusCode != 0 && (first.StatusCode < 200 || first.StatusCode >= 300) {
		fmt.Fprintf(os.Stderr, "\nupstream returned HTTP %d\n", first.StatusCode)
	}
	return nil
}

// resolveTarget fills in -service and -rpc-type from the config when they are
// unambiguous, so the common single-app case needs neither flag.
//
// The app set is consulted before the listeners: a single app names exactly one
// service, which is a better default than a listener's — it holds for a config
// with no listeners at all, which is the shape a call-only config has.
func resolveTarget(cfg *config.Config, apps []resolvedApp, service, rpcTypeStr string) (domain.ServiceID, domain.RPCType, error) {
	serviceID := domain.ServiceID(service)
	switch {
	case serviceID != "":
	case len(apps) == 1:
		serviceID = apps[0].serviceID
	case len(cfg.Listeners) == 1:
		serviceID = domain.ServiceID(cfg.Listeners[0].ServiceID)
	default:
		return "", domain.RPCTypeUnknown, fmt.Errorf(
			"call: -service is required (%d apps and %d listeners are configured, so there is no unambiguous default)",
			len(apps), len(cfg.Listeners))
	}
	if serviceID == "" {
		return "", domain.RPCTypeUnknown, fmt.Errorf("call: -service is required (the matching listener declares no service_id)")
	}

	var rpcType domain.RPCType
	if rpcTypeStr != "" {
		var err error
		if rpcType, err = domain.ParseRPCType(rpcTypeStr); err != nil {
			return "", domain.RPCTypeUnknown, fmt.Errorf("call: %w", err)
		}
	} else {
		// Default the type from the listeners serving this service — safe only
		// when exactly one does.
		var matched []domain.RPCType
		for _, l := range cfg.Listeners {
			lServiceID, lRPCType, err := l.Parsed()
			if err != nil {
				return "", domain.RPCTypeUnknown, err
			}
			if lServiceID == serviceID {
				matched = append(matched, lRPCType)
			}
		}
		if len(matched) != 1 {
			return "", domain.RPCTypeUnknown, fmt.Errorf(
				"call: -rpc-type is required (%d listeners serve %s, so there is no unambiguous default)", len(matched), serviceID)
		}
		rpcType = matched[0]
	}

	if !rpcType.Stateless() {
		return "", domain.RPCTypeUnknown, fmt.Errorf("call: %s is a streaming type, which has no one-shot form", rpcType)
	}
	return serviceID, rpcType, nil
}

// readBody resolves -d: literal text, "@file", "-" for stdin, or empty.
func readBody(data string) ([]byte, error) {
	switch {
	case data == "":
		return nil, nil
	case data == "-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(data, "@"):
		return os.ReadFile(strings.TrimPrefix(data, "@"))
	default:
		return []byte(data), nil
	}
}

// resolveMethod follows curl: POST when there is a body, GET when there is not.
func resolveMethod(explicit string, body []byte) string {
	if explicit != "" {
		return strings.ToUpper(explicit)
	}
	if len(body) > 0 {
		return http.MethodPost
	}
	return http.MethodGet
}

// directResult is the outcome of the -compare baseline request.
type directResult struct {
	status  int
	body    []byte
	latency time.Duration
	err     error
}

// sendDirect issues the same request straight to a URL, bypassing Pocket
// entirely — no session, no signing, no supplier. It is the -compare baseline,
// there to answer "is a bad response the relay's fault or the backend's?".
//
// The headers and body are sent verbatim, exactly as the relay carries them, so
// the two sides stay comparable. Nothing is added on this path that the relay
// path would not also send.
func sendDirect(ctx context.Context, baseURL string, in domain.RelayInput) directResult {
	start := time.Now()

	u, err := joinURL(baseURL, in.Path)
	if err != nil {
		return directResult{err: err, latency: time.Since(start)}
	}
	req, err := http.NewRequestWithContext(ctx, in.Method, u, bytes.NewReader(in.Body))
	if err != nil {
		return directResult{err: err, latency: time.Since(start)}
	}
	for name, values := range in.Header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return directResult{err: err, latency: time.Since(start)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	return directResult{status: resp.StatusCode, body: body, latency: time.Since(start), err: err}
}

// joinURL appends the request path to the baseline's base URL, keeping any path
// the base already carries (e.g. the "/eth" in "https://host/eth").
func joinURL(baseURL, path string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse -compare url: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("-compare url %q needs a scheme and host", baseURL)
	}
	rel, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("parse -path: %w", err)
	}
	if rel.Path != "" && rel.Path != "/" {
		base.Path = strings.TrimSuffix(base.Path, "/") + rel.Path
	}
	base.RawQuery = rel.RawQuery
	return base.String(), nil
}

// jsonEquivalent reports whether two bodies are the same JSON value that differ
// only in key order or whitespace. False if either side is not a single JSON
// value — protobuf, plain text and anything else fall back to the byte verdict.
//
// This is the one place that looks inside a payload, and it is deliberately
// confined: it runs only in "call -compare", only to label a diff for a human,
// and it feeds nothing back into routing, selection, or the relay path. The
// opaque-bytes rule governs how relays are carried; it is not violated by a
// debug tool describing why two bodies it already fetched look different.
// Generic JSON is also not a chain format — nothing here knows what eth_call is.
func jsonEquivalent(a, b []byte) bool {
	var av, bv any
	if decodeJSONValue(a, &av) != nil || decodeJSONValue(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// decodeJSONValue decodes exactly one JSON value, keeping numbers as their
// literal text so 1.0 and 1 do not collapse into the same float.
func decodeJSONValue(data []byte, v *any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after the first JSON value")
	}
	return nil
}

// dumpCompare reports the relay against the baseline. The verdict is byte-exact
// first — a genuine passthrough serving the same backend should be byte-identical,
// so that distinction is worth keeping — then falls back to a JSON-equivalence
// check to defuse the false alarm of two backends ordering their keys differently.
func dumpCompare(w io.Writer, rec *recorder, compareURL string, result *domain.RelayResult, relayErr error, direct directResult) {
	host := compareURL
	if u, err := url.Parse(compareURL); err == nil && u.Host != "" {
		host = u.Host
	}

	fmt.Fprintf(w, "--- comparison ---\n")
	if relayErr != nil {
		fmt.Fprintf(w, "via pocket:  failed: %v\n", relayErr)
	} else {
		supplier, latency := rec.lastAttempt()
		fmt.Fprintf(w, "via pocket:  %s in %s, HTTP %d, %d bytes\n",
			supplier, latency.Round(time.Millisecond), result.StatusCode, len(result.Body))
	}
	if direct.err != nil {
		fmt.Fprintf(w, "via direct:  %s failed: %v\n", host, direct.err)
	} else {
		fmt.Fprintf(w, "via direct:  %s in %s, HTTP %d, %d bytes\n",
			host, direct.latency.Round(time.Millisecond), direct.status, len(direct.body))
	}

	switch {
	case relayErr != nil || direct.err != nil:
		fmt.Fprintf(w, "bodies:      not compared, one side failed\n")
	case bytes.Equal(result.Body, direct.body):
		fmt.Fprintf(w, "bodies:      identical\n")
	case jsonEquivalent(result.Body, direct.body):
		fmt.Fprintf(w, "bodies:      equivalent — same JSON value, only key order or whitespace differs\n")
	default:
		fmt.Fprintf(w, "bodies:      differ\n")
		fmt.Fprintf(w, "  pocket:    %s\n", preview(result.Body))
		fmt.Fprintf(w, "  direct:    %s\n", preview(direct.body))
		fmt.Fprintf(w, "note:        a difference is not necessarily a bug. Non-deterministic methods\n")
		fmt.Fprintf(w, "             (eth_blockNumber, timestamps) move between calls, and the two\n")
		fmt.Fprintf(w, "             backends may be different nodes entirely. A mismatch is most\n")
		fmt.Fprintf(w, "             telling when one side errors and the other succeeds, or the\n")
		fmt.Fprintf(w, "             statuses disagree — those point at passthrough bugs rather\n")
		fmt.Fprintf(w, "             than backend drift.\n")
	}
}

// preview truncates a body for side-by-side display.
func preview(body []byte) string {
	const max = 200
	s := strings.TrimSpace(string(body))
	if len(s) > max {
		return fmt.Sprintf("%s... (%d bytes total)", s[:max], len(body))
	}
	return s
}

// recorder captures what the relay did, filled in by the seam wrappers below.
type recorder struct {
	mu       sync.Mutex
	session  *domain.Session
	ordered  []domain.Endpoint
	attempts []attempt
}

type attempt struct {
	supplier domain.EndpointAddr
	latency  time.Duration
	err      error
}

func (r *recorder) setSession(s *domain.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session = s
}

func (r *recorder) setOrdered(eps []domain.Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ordered = eps
}

func (r *recorder) addAttempt(a attempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts = append(r.attempts, a)
}

// lastAttempt returns the supplier and latency of the final relay attempt — the
// one that produced the response, when the relay succeeded.
func (r *recorder) lastAttempt() (domain.EndpointAddr, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.attempts) == 0 {
		return "unknown-supplier", 0
	}
	a := r.attempts[len(r.attempts)-1]
	return a.supplier, a.latency
}

// dump writes the human-readable diagnostics for -v.
func (r *recorder) dump(w io.Writer, serviceID domain.ServiceID, rpcType domain.RPCType, elapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(w, "--- relay diagnostics ---\n")
	fmt.Fprintf(w, "service:   %s (%s)\n", serviceID, rpcType)
	if r.session != nil {
		fmt.Fprintf(w, "app:       %s\n", r.session.AppAddr)
		fmt.Fprintf(w, "session:   %s (ends at block %d)\n", r.session.ID, r.session.EndBlockHeight)
		fmt.Fprintf(w, "endpoints: %d in session, %d support %s\n", len(r.session.Endpoints), len(r.ordered), rpcType)
	}
	for i, a := range r.attempts {
		outcome := "ok"
		if a.err != nil {
			outcome = a.err.Error()
		}
		fmt.Fprintf(w, "attempt %d: %s in %s via %s -> %s\n",
			i+1, a.supplier, a.latency.Round(time.Millisecond), r.urlFor(a.supplier, rpcType), outcome)
	}
	fmt.Fprintf(w, "total:     %s\n", elapsed.Round(time.Millisecond))
}

// urlFor recovers the URL a supplier advertised for this type. The outcome feed
// reports the supplier, not the URL, so the endpoint list is what maps it back.
// Caller holds r.mu.
func (r *recorder) urlFor(supplier domain.EndpointAddr, rpcType domain.RPCType) string {
	for _, ep := range r.ordered {
		if ep.Supplier == supplier {
			if u, ok := ep.URL(rpcType); ok {
				return u
			}
		}
	}
	return "unknown-url"
}

// The three wrappers below implement the relay seams by delegating and noting
// what passed through.

type recSessions struct {
	inner relay.SessionSource
	rec   *recorder
}

func (r recSessions) Session(ctx context.Context, serviceID domain.ServiceID) (*domain.Session, error) {
	s, err := r.inner.Session(ctx, serviceID)
	if err == nil {
		r.rec.setSession(s)
	}
	return s, err
}

func (r recSessions) Start(ctx context.Context) error { return r.inner.Start(ctx) }

// recSelector implements both relay.Selector and relay.Observer: it records what
// was picked, then records how each pick turned out. This is the same optional
// hook a QoS selector would use to learn — "call -v" just prints the feed
// instead of scoring it, which makes it the working proof the hook is usable.
type recSelector struct {
	inner relay.Selector
	rec   *recorder
}

// Compile-time assertion: the Relayer only calls Observe if this holds.
var _ relay.Observer = recSelector{}

func (r recSelector) Select(ctx context.Context, serviceID domain.ServiceID, endpoints []domain.Endpoint, rpcType domain.RPCType) ([]domain.Endpoint, error) {
	ordered, err := r.inner.Select(ctx, serviceID, endpoints, rpcType)
	if err == nil {
		r.rec.setOrdered(ordered)
	}
	return ordered, err
}

// Observe implements relay.Observer. It only appends, so it does not block the
// relay — the contract every Observer has to keep.
func (r recSelector) Observe(supplier domain.EndpointAddr, o relay.Outcome) {
	r.rec.addAttempt(attempt{supplier: supplier, latency: o.Latency, err: o.Err})
}
