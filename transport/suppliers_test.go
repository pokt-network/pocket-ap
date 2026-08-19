package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pokt-network/pocket-ap/domain"
)

func TestTakeSupplierPolicy_ParsesBothLists(t *testing.T) {
	header := map[string][]string{
		HeaderAllowSuppliers: {"pokt1aaa, pokt1bbb"},
		HeaderDenySuppliers:  {"pokt1ccc"},
	}

	got, err := TakeSupplierPolicy(header)
	if err != nil {
		t.Fatalf("TakeSupplierPolicy: %v", err)
	}
	if len(got.Allow) != 2 || got.Allow[0] != "pokt1aaa" || got.Allow[1] != "pokt1bbb" {
		t.Errorf("allow = %v, want [pokt1aaa pokt1bbb]", got.Allow)
	}
	if len(got.Deny) != 1 || got.Deny[0] != "pokt1ccc" {
		t.Errorf("deny = %v, want [pokt1ccc]", got.Deny)
	}
}

// Repeating a header is the other natural way to write a list, and net/http
// keeps every value. Dropping all but the first would silently route to a subset
// of what the caller asked for.
func TestTakeSupplierPolicy_RepeatedHeadersAccumulate(t *testing.T) {
	header := map[string][]string{
		HeaderAllowSuppliers: {"pokt1aaa", "pokt1bbb,pokt1ccc"},
	}

	got, err := TakeSupplierPolicy(header)
	if err != nil {
		t.Fatalf("TakeSupplierPolicy: %v", err)
	}
	if len(got.Allow) != 3 {
		t.Errorf("allow = %v, want 3 entries", got.Allow)
	}
}

// The call CLI builds its header map from "-H Name: value" verbatim, so the keys
// are whatever the user typed rather than net/http's canonical form. Matching on
// the exact string would make the header work through serve and silently do
// nothing through call.
func TestTakeSupplierPolicy_MatchesHeaderNameCaseInsensitively(t *testing.T) {
	header := map[string][]string{"x-pocket-allow-suppliers": {"pokt1aaa"}}

	got, err := TakeSupplierPolicy(header)
	if err != nil {
		t.Fatalf("TakeSupplierPolicy: %v", err)
	}
	if len(got.Allow) != 1 {
		t.Fatalf("allow = %v, want 1 entry", got.Allow)
	}
	if _, still := header["x-pocket-allow-suppliers"]; still {
		t.Error("header survived the take: it would be signed and replayed to the backend")
	}
}

// The headers address this proxy. Anything left in the map is signed into the
// relay and replayed to the backend, which would tell the supplier which of its
// competitors the caller ranked.
func TestTakeSupplierPolicy_RemovesTheHeaders(t *testing.T) {
	header := map[string][]string{
		HeaderAllowSuppliers: {"pokt1aaa"},
		HeaderDenySuppliers:  {"pokt1bbb"},
		"Content-Type":       {"application/json"},
	}

	if _, err := TakeSupplierPolicy(header); err != nil {
		t.Fatalf("TakeSupplierPolicy: %v", err)
	}
	if len(header) != 1 || header["Content-Type"][0] != "application/json" {
		t.Errorf("header = %v, want only Content-Type left", header)
	}
}

// A typo is silent and expensive: an address that never matches makes an
// allowlist drop every supplier and a denylist deny nobody. Config catches this
// at startup; a request can be told, so it must be.
func TestTakeSupplierPolicy_RejectsNonOperatorAddress(t *testing.T) {
	header := map[string][]string{HeaderAllowSuppliers: {"pokt1aaa,0xdeadbeef"}}

	_, err := TakeSupplierPolicy(header)
	if err == nil {
		t.Fatal("want an error for an address that is not pokt1…")
	}
	if !strings.Contains(err.Error(), "0xdeadbeef") {
		t.Errorf("error %q does not name the offending value", err)
	}
}

func TestTakeSupplierPolicy_NoHeadersIsTheZeroPolicy(t *testing.T) {
	got, err := TakeSupplierPolicy(map[string][]string{"Content-Type": {"application/json"}})
	if err != nil {
		t.Fatalf("TakeSupplierPolicy: %v", err)
	}
	if !got.Empty() {
		t.Errorf("policy = %+v, want the zero policy", got)
	}
}

// Empty fields are formatting slips ("a,,b", a trailing comma) and mean nothing.
// Erroring on them would reject a request over a stray character.
func TestTakeSupplierPolicy_IgnoresEmptyFields(t *testing.T) {
	got, err := TakeSupplierPolicy(map[string][]string{HeaderAllowSuppliers: {"pokt1aaa, ,pokt1bbb,"}})
	if err != nil {
		t.Fatalf("TakeSupplierPolicy: %v", err)
	}
	if len(got.Allow) != 2 {
		t.Errorf("allow = %v, want 2 entries", got.Allow)
	}
}

// capturingRelay records the ctx and the headers the relay core was handed —
// the two things the front adapter is responsible for here.
type capturingRelay struct {
	policy domain.SupplierPolicy
	header map[string][]string
	calls  int
}

func (c *capturingRelay) fn(ctx context.Context, _ domain.ServiceID, _ domain.RPCType, in domain.RelayInput) (*domain.RelayResult, error) {
	c.calls++
	c.policy = domain.SupplierPolicyFromContext(ctx)
	c.header = in.Header
	return &domain.RelayResult{StatusCode: 200, Body: []byte(`{}`)}, nil
}

// End to end through the handler: the header must reach the Selector (via ctx)
// and must NOT reach the supplier (via in.Header).
func TestHTTP_SupplierHeaderReachesSelectionAndNotTheBackend(t *testing.T) {
	relay := &capturingRelay{}
	h := newHTTPForTest(relay.fn, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(HeaderAllowSuppliers, "pokt1aaa")
	req.Header.Set(HeaderDenySuppliers, "pokt1bbb")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if len(relay.policy.Allow) != 1 || relay.policy.Allow[0] != "pokt1aaa" {
		t.Errorf("allow = %v, want [pokt1aaa]", relay.policy.Allow)
	}
	if len(relay.policy.Deny) != 1 || relay.policy.Deny[0] != "pokt1bbb" {
		t.Errorf("deny = %v, want [pokt1bbb]", relay.policy.Deny)
	}
	for name := range relay.header {
		if strings.EqualFold(name, HeaderAllowSuppliers) || strings.EqualFold(name, HeaderDenySuppliers) {
			t.Errorf("%s was forwarded into the signed relay", name)
		}
	}
}

// A malformed list is an instruction we cannot honour. Relaying anyway would
// route to suppliers the caller told us to avoid, and bill them for it.
func TestHTTP_MalformedSupplierHeaderCostsNoRelay(t *testing.T) {
	relay := &countingRelay{}
	h := newHTTPForTest(relay.fn, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(HeaderAllowSuppliers, "not-an-address")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
	if n := relay.calls.Load(); n != 0 {
		t.Errorf("%d relays spent on a request we could not honour", n)
	}
}

// --- host headers ---

func TestTakeSupplierPolicy_ParsesHostLists(t *testing.T) {
	header := map[string][]string{
		HeaderAllowHosts: {"rm.example.com:443, *.other.example"},
		HeaderDenyHosts:  {"bad.example.com"},
	}

	got, err := TakeSupplierPolicy(header)
	if err != nil {
		t.Fatalf("TakeSupplierPolicy: %v", err)
	}
	if len(got.AllowHosts) != 2 || got.AllowHosts[0] != "rm.example.com:443" {
		t.Errorf("allow_hosts = %v", got.AllowHosts)
	}
	if len(got.DenyHosts) != 1 || got.DenyHosts[0] != "bad.example.com" {
		t.Errorf("deny_hosts = %v", got.DenyHosts)
	}
	if len(header) != 0 {
		t.Errorf("header = %v, want the host headers taken out too", header)
	}
}

// A URL is the obvious thing to paste, can never match a parsed host, and as a
// denylist entry would fail open. It has to be refused, not ignored.
func TestTakeSupplierPolicy_RejectsAURLInAHostList(t *testing.T) {
	_, err := TakeSupplierPolicy(map[string][]string{HeaderDenyHosts: {"https://bad.example.com/"}})
	if err == nil {
		t.Fatal("want an error for a URL in a host list")
	}
	if !strings.Contains(err.Error(), HeaderDenyHosts) {
		t.Errorf("error %q does not name the header at fault", err)
	}
}

// The mirror: an address in the host list matches nothing, a host in the address
// list matches nothing. Each has to be caught by the list it was put in.
func TestTakeSupplierPolicy_ListsRejectEachOthersContent(t *testing.T) {
	if _, err := TakeSupplierPolicy(map[string][]string{HeaderAllowHosts: {"pokt1abc"}}); err == nil {
		t.Error("an operator address was accepted in a host list")
	}
	if _, err := TakeSupplierPolicy(map[string][]string{HeaderAllowSuppliers: {"rm.example.com"}}); err == nil {
		t.Error("a host was accepted in an address list")
	}
}

// A rejected request must not leave the offending header behind for the next
// stage to sign — every list is taken before the error is returned.
// Each list is checked as the bad one, because a short-circuit on the FIRST
// failure leaves every later header sitting in the map — and the map is what
// gets signed. A single case would only catch a short-circuit on that one list.
func TestTakeSupplierPolicy_TakesEveryHeaderEvenWhenOneIsBad(t *testing.T) {
	bad := map[string]string{
		HeaderAllowSuppliers: "not-an-address",
		HeaderDenySuppliers:  "not-an-address",
		HeaderAllowHosts:     "https://bad.example.com/",
		HeaderDenyHosts:      "https://bad.example.com/",
	}
	for badHeader, badValue := range bad {
		t.Run(badHeader, func(t *testing.T) {
			header := map[string][]string{
				HeaderAllowSuppliers: {"pokt1aaa"},
				HeaderDenySuppliers:  {"pokt1bbb"},
				HeaderAllowHosts:     {"good.example.com"},
				HeaderDenyHosts:      {"bad.example.com"},
				"Content-Type":       {"application/json"},
			}
			header[badHeader] = []string{badValue}

			if _, err := TakeSupplierPolicy(header); err == nil {
				t.Fatal("want an error")
			}
			if len(header) != 1 || header["Content-Type"] == nil {
				t.Errorf("header = %v, want every supplier header removed despite the error", header)
			}
		})
	}
}

func TestHTTP_HostHeaderReachesSelection(t *testing.T) {
	relay := &capturingRelay{}
	h := newHTTPForTest(relay.fn, nil)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(HeaderDenyHosts, "*.bad.example")
	rec := httptest.NewRecorder()

	h.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if len(relay.policy.DenyHosts) != 1 || relay.policy.DenyHosts[0] != "*.bad.example" {
		t.Errorf("deny_hosts = %v", relay.policy.DenyHosts)
	}
	for name := range relay.header {
		if strings.EqualFold(name, HeaderDenyHosts) {
			t.Errorf("%s was forwarded into the signed relay", name)
		}
	}
}
