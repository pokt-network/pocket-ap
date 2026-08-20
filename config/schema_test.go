package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// config.schema.yaml is a published contract: editors validate against it and
// AGENTS.md points at it as the machine-readable source of truth. A schema that
// has drifted from the structs is worse than no schema — it documents fields
// that do not exist and hides ones that do, and nothing else would ever catch
// it, because the schema is never loaded by the program.
//
// So: walk the schema and the structs, and require they agree exactly.

type schemaNode struct {
	Type                 string                `yaml:"type"`
	Properties           map[string]schemaNode `yaml:"properties"`
	Items                *schemaNode           `yaml:"items"`
	Required             []string              `yaml:"required"`
	AdditionalProperties *bool                 `yaml:"additionalProperties"`
}

func loadSchema(t *testing.T) schemaNode {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "config.schema.yaml"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var s schemaNode
	if err := yaml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("config.schema.yaml is not valid YAML: %v", err)
	}
	return s
}

// yamlFields returns the yaml tag names declared on a struct.
func yamlFields(v any) []string {
	rt := reflect.TypeOf(v)
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, strings.Split(tag, ",")[0])
	}
	sort.Strings(out)
	return out
}

func keys(m map[string]schemaNode) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func requireSameFields(t *testing.T, what string, schemaProps map[string]schemaNode, structVal any) {
	t.Helper()
	got, want := keys(schemaProps), yamlFields(structVal)

	inSchema := map[string]bool{}
	for _, k := range got {
		inSchema[k] = true
	}
	inStruct := map[string]bool{}
	for _, k := range want {
		inStruct[k] = true
	}

	for _, k := range want {
		if !inSchema[k] {
			t.Errorf("%s: field %q exists in the config struct but is MISSING from config.schema.yaml — it is undocumented and editors will flag it as unknown", what, k)
		}
	}
	for _, k := range got {
		if !inStruct[k] {
			t.Errorf("%s: config.schema.yaml documents %q, which the config struct does NOT have — anyone who sets it gets an 'unknown field' error at load", what, k)
		}
	}
}

func TestSchemaMatchesConfigStructs(t *testing.T) {
	s := loadSchema(t)

	t.Run("top level", func(t *testing.T) {
		requireSameFields(t, "Config", s.Properties, Config{})
	})
	t.Run("app", func(t *testing.T) {
		requireSameFields(t, "App", s.Properties["app"].Properties, App{})
	})
	t.Run("fullnode", func(t *testing.T) {
		requireSameFields(t, "FullNode", s.Properties["fullnode"].Properties, FullNode{})
	})
	t.Run("admin", func(t *testing.T) {
		requireSameFields(t, "Admin", s.Properties["admin"].Properties, Admin{})
	})
	t.Run("apps[]", func(t *testing.T) {
		items := s.Properties["apps"].Items
		if items == nil {
			t.Fatal("schema: apps has no items definition")
		}
		requireSameFields(t, "App", items.Properties, App{})
	})
	t.Run("app.suppliers", func(t *testing.T) {
		requireSameFields(t, "SupplierPolicy", s.Properties["app"].Properties["suppliers"].Properties, SupplierPolicy{})
	})
	t.Run("listeners[]", func(t *testing.T) {
		items := s.Properties["listeners"].Items
		if items == nil {
			t.Fatal("schema: listeners has no items definition")
		}
		requireSameFields(t, "Listener", items.Properties, Listener{})
	})
}

// The loader is strict (KnownFields(true)), so the schema must be strict too —
// otherwise it would accept a config that the program then rejects at startup,
// which is the worst possible split between a contract and its implementation.
func TestSchemaIsStrictWhereTheLoaderIs(t *testing.T) {
	s := loadSchema(t)

	check := func(name string, n schemaNode) {
		t.Helper()
		if n.AdditionalProperties == nil || *n.AdditionalProperties {
			t.Errorf("schema: %s allows additional properties, but config.Load uses KnownFields(true) and will reject them", name)
		}
	}
	check("top level", s)
	check("app", s.Properties["app"])
	check("fullnode", s.Properties["fullnode"])
	check("admin", s.Properties["admin"])
	check("app.suppliers", s.Properties["app"].Properties["suppliers"])
	if items := s.Properties["apps"].Items; items != nil {
		check("apps[]", *items)
	}
	if items := s.Properties["listeners"].Items; items != nil {
		check("listeners[]", *items)
	}
}

// Anything the schema marks required must actually be enforced by validate(),
// or the schema is promising something the program does not check.
func TestSchemaRequiredFieldsAreEnforced(t *testing.T) {
	s := loadSchema(t)

	fullnodeRequired := map[string]bool{}
	for _, r := range s.Properties["fullnode"].Required {
		fullnodeRequired[r] = true
	}
	for _, want := range []string{"grpc_host_port", "rpc_url"} {
		if !fullnodeRequired[want] {
			t.Errorf("schema: fullnode.%s is enforced by validate() but not marked required", want)
		}
	}

	// listeners[] required fields must match what validate() actually demands.
	items := s.Properties["listeners"].Items
	if items == nil {
		t.Fatal("schema: listeners has no items")
	}
	listenerRequired := map[string]bool{}
	for _, r := range items.Required {
		listenerRequired[r] = true
	}
	for _, want := range []string{"addr", "rpc_type"} {
		if !listenerRequired[want] {
			t.Errorf("schema: listeners[].%s is enforced by validate() but not marked required", want)
		}
	}
	// The converse: service_id is deliberately NOT required — it is derivable
	// from the app key, and a single-app config leaves it out. A schema that
	// demanded it would make every editor flag a config the program accepts.
	if listenerRequired["service_id"] {
		t.Error("schema: listeners[].service_id is marked required, but validate() allows it to be empty (one app => one service, so it is derivable)")
	}
}

// Every rpc_type the schema offers must actually parse. A schema that suggests a
// value the program rejects is a trap: the editor says it is fine, startup says
// it is not.
func TestSchemaRPCTypeEnumAllParse(t *testing.T) {
	s := loadSchema(t)

	items := s.Properties["listeners"].Items
	if items == nil {
		t.Fatal("schema: listeners has no items")
	}

	// Re-read the raw node for the enum: schemaNode does not model it.
	raw, err := os.ReadFile(filepath.Join("..", "config.schema.yaml"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var doc struct {
		Properties struct {
			Listeners struct {
				Items struct {
					Properties struct {
						RPCType struct {
							Enum []string `yaml:"enum"`
						} `yaml:"rpc_type"`
					} `yaml:"properties"`
				} `yaml:"items"`
			} `yaml:"listeners"`
		} `yaml:"properties"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	enum := doc.Properties.Listeners.Items.Properties.RPCType.Enum
	if len(enum) == 0 {
		t.Fatal("schema: rpc_type has no enum — every value would look valid")
	}
	for _, v := range enum {
		t.Run(v, func(t *testing.T) {
			if _, _, err := (Listener{RPCType: v}).Parsed(); err != nil {
				t.Errorf("schema offers rpc_type %q but the program rejects it: %v", v, err)
			}
		})
	}
}

// The public full-node endpoints are written in FOUR places: config.example.yaml
// (what everyone copies), config.schema.yaml (what editors read), README.md and
// AGENTS.md (what humans and agents read). Nothing loads three of those, so drift
// between them is invisible.
//
// This is not hypothetical. config.example.yaml shipped pointing at the
// generation of beta hosts BEFORE sauron, long after those stopped resolving,
// while config.schema.yaml already listed the sauron ones — so the file every
// new user copies gave them a proxy that could not reach a full node, and the
// schema beside it disagreed. The dead names are not repeated here: they resolve
// to nothing, so writing them down only gives someone something to paste.
//
// No test can tell that an endpoint is DEAD without the network, and these will
// rotate again. What a test can do is make the next rotation loud: change the
// example and this fails until the other three agree.
func TestExampleEndpointsAreDocumentedEverywhere(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "4bd7f2e1a9c3068b5d4f7e2a1c9b8d6e3f5a7c2b4d6e8f0a1c3e5b7d9f2a4c6e")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	raw, err := os.ReadFile(filepath.Join("..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	cfg, err := Load(write(t, string(raw)))
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}

	// The example must not point at a local node: nobody copying it has one.
	for field, val := range map[string]string{
		"fullnode.grpc_host_port": cfg.FullNode.GRPCHostPort,
		"fullnode.rpc_url":        cfg.FullNode.RPCURL,
	} {
		if strings.Contains(val, "localhost") || strings.Contains(val, "127.0.0.1") {
			t.Errorf("config.example.yaml %s is %q — the example must point at a public full node, or everyone who copies it needs one running first", field, val)
		}
	}
	// Plaintext to a public endpoint would ship everyone's queries in the clear.
	if cfg.FullNode.GRPCInsecure {
		t.Error("config.example.yaml sets grpc_insecure: true against a public endpoint — that sends queries in plaintext")
	}

	for _, doc := range []string{"config.schema.yaml", "README.md", "AGENTS.md"} {
		body, err := os.ReadFile(filepath.Join("..", doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for field, val := range map[string]string{
			"fullnode.grpc_host_port": cfg.FullNode.GRPCHostPort,
			"fullnode.rpc_url":        cfg.FullNode.RPCURL,
		} {
			if !strings.Contains(string(body), val) {
				t.Errorf("%s does not mention %s (%q) from config.example.yaml — the endpoints are duplicated across the example, the schema, README.md and AGENTS.md, so updating one means updating all four", doc, field, val)
			}
		}
	}
}

// The example config is what everyone copies, so it must satisfy the loader —
// including the schema modeline comment at the top, which must not upset the
// strict parser.
func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("POCKET_APP_PRIVATE_KEY", "4bd7f2e1a9c3068b5d4f7e2a1c9b8d6e3f5a7c2b4d6e8f0a1c3e5b7d9f2a4c6e")
	t.Setenv("POCKET_APP_PRIVATE_KEYS", "")

	raw, err := os.ReadFile(filepath.Join("..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	cfg, err := Load(write(t, string(raw)))
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}
	if len(cfg.Listeners) == 0 {
		t.Error("the example config has no listeners — copying it gives you a proxy that cannot serve")
	}
	// The example is what people copy; it must not hand them a LAN-exposed relay.
	for _, l := range cfg.Listeners {
		if !strings.HasPrefix(l.Addr, "127.0.0.1:") && !strings.HasPrefix(l.Addr, "localhost:") {
			t.Errorf("the example config binds %q — it must bind loopback, or everyone who copies it exposes their stake to their network", l.Addr)
		}
	}
}

// The README pins a version in five places: two ghcr pulls, the release-archive
// snippet, the raw.githubusercontent URL its config.example.yaml comes from, and
// the `docker create` that copies that file out of the image. Every one of them
// is a literal, and a literal that nothing checks is a literal that survives the
// next release unchanged — pointing a new user at artifacts for a version that is
// no longer the current one.
//
// The CHANGELOG's newest released heading is the source of truth here because it
// is a tracked file: reading the newest git tag instead would make this test need
// a repository and a network, and `GOPROXY=off` builds are something this repo
// keeps working on purpose.
func TestREADMEVersionMatchesChangelog(t *testing.T) {
	changelog, err := os.ReadFile(filepath.Join("..", "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	// "## [0.1.1] - 2026-08-20" — the first such heading that is not [Unreleased].
	released := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindSubmatch(changelog)
	if released == nil {
		t.Fatal("CHANGELOG.md has no released version heading of the form '## [x.y.z]'")
	}
	want := "v" + string(released[1])

	readme, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	// Scoped to OUR artifacts on purpose. A bare version regex also matches
	// poktroll's v0.1.34, which the docs name for an entirely different reason
	// and which must not move when pocket-ap releases. The first draft of this
	// test did exactly that and failed on it.
	pinned := regexp.MustCompile(`(?m)pokt-network/pocket-ap[:/](v\d+\.\d+\.\d+)|^VERSION=(v\d+\.\d+\.\d+)`)
	matches := pinned.FindAllStringSubmatch(string(readme), -1)
	if len(matches) == 0 {
		t.Fatalf("README.md pins no pocket-ap version at all — it should reference %s where it names artifacts", want)
	}
	for _, m := range matches {
		got := m[1]
		if got == "" {
			got = m[2]
		}
		if got != want {
			t.Errorf("README.md references pocket-ap %s but CHANGELOG.md's newest release is %s — every pinned version in the README has to move with a release, or it points at artifacts that are no longer current", got, want)
		}
	}
}
