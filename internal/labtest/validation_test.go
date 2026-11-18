package labtest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"kubix/internal/agent"
	"kubix/internal/core"
)

func readerFixture() *clientcmdapi.Config {
	claims, _ := json.Marshal(map[string]any{"sub": "system:serviceaccount:kubix-lab:kubix-reader", "exp": time.Now().Add(time.Hour).Unix()})
	return &clientcmdapi.Config{
		CurrentContext: labContext,
		Contexts:       map[string]*clientcmdapi.Context{labContext: {Cluster: "kind-kubix-lab", AuthInfo: "kubix-reader", Namespace: labNamespace}},
		Clusters:       map[string]*clientcmdapi.Cluster{"kind-kubix-lab": {Server: "https://127.0.0.1:53499", CertificateAuthorityData: []byte("fixture-ca")}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"kubix-reader": {Token: "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"}},
	}
}

func TestReaderConfigRejectsAdministratorAndRemoteCredentials(t *testing.T) {
	if err := validateReaderConfig(readerFixture()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*clientcmdapi.Config){
		"remote API":        func(c *clientcmdapi.Config) { c.Clusters["kind-kubix-lab"].Server = "https://example.com" },
		"proxy":             func(c *clientcmdapi.Config) { c.Clusters["kind-kubix-lab"].ProxyURL = "https://proxy.example.com" },
		"skip TLS":          func(c *clientcmdapi.Config) { c.Clusters["kind-kubix-lab"].InsecureSkipTLSVerify = true },
		"missing CA":        func(c *clientcmdapi.Config) { c.Clusters["kind-kubix-lab"].CertificateAuthorityData = nil },
		"admin certificate": func(c *clientcmdapi.Config) { c.AuthInfos["kubix-reader"].ClientCertificateData = []byte("admin") },
		"exec authentication": func(c *clientcmdapi.Config) {
			c.AuthInfos["kubix-reader"].Exec = &clientcmdapi.ExecConfig{Command: "admin-helper"}
		},
		"impersonation": func(c *clientcmdapi.Config) { c.AuthInfos["kubix-reader"].Impersonate = "admin" },
		"impersonation groups": func(c *clientcmdapi.Config) {
			c.AuthInfos["kubix-reader"].ImpersonateGroups = []string{"system:masters"}
		},
		"impersonation extra": func(c *clientcmdapi.Config) {
			c.AuthInfos["kubix-reader"].ImpersonateUserExtra = map[string][]string{"scope": {"admin"}}
		},
		"username": func(c *clientcmdapi.Config) { c.AuthInfos["kubix-reader"].Username = "admin" },
		"auth provider": func(c *clientcmdapi.Config) {
			c.AuthInfos["kubix-reader"].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "oidc"}
		},
		"extension": func(c *clientcmdapi.Config) {
			c.AuthInfos["kubix-reader"].Extensions = map[string]runtime.Object{"unexpected": &runtime.Unknown{Raw: []byte(`{}`)}}
		},
		"other context":   func(c *clientcmdapi.Config) { c.Contexts["prod"] = &clientcmdapi.Context{} },
		"other namespace": func(c *clientcmdapi.Config) { c.Contexts[labContext].Namespace = "prod" },
		"missing token":   func(c *clientcmdapi.Config) { c.AuthInfos["kubix-reader"].Token = "" },
		"wrong token subject": func(c *clientcmdapi.Config) {
			c.AuthInfos["kubix-reader"].Token = "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin","exp":9999999999}`)) + ".s"
		},
		"expired token": func(c *clientcmdapi.Config) {
			c.AuthInfos["kubix-reader"].Token = "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:kubix-lab:kubix-reader","exp":1}`)) + ".s"
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := readerFixture()
			mutate(c)
			if err := validateReaderConfig(c); err == nil {
				t.Fatal("unsafe lab configuration accepted")
			}
		})
	}
}

func TestReaderConfigAcceptsSerializedTokenOnlyConfig(t *testing.T) {
	encoded, err := clientcmd.Write(*readerFixture())
	if err != nil {
		t.Fatal("could not serialize fixture kubeconfig")
	}
	loaded, err := clientcmd.Load(encoded)
	if err != nil {
		t.Fatal("could not load fixture kubeconfig")
	}
	if err := validateReaderConfig(loaded); err != nil {
		t.Fatalf("real loader rejected token-only credentials: %v", err)
	}
}

func TestModelAndReportDestinationsStayLocal(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1:11435/v1", "http://localhost:11435/v1/", "http://[::1]:11435/v1"} {
		if !loopbackURL(endpoint, "", "/v1") {
			t.Errorf("local URL rejected: %s", endpoint)
		}
	}
	for _, endpoint := range []string{"https://api.example.com/v1", "http://localhost.example.com/v1", "http://192.168.1.2/v1", "http://user:pass@127.0.0.1/v1", "http://127.0.0.1/v1?token=x", "http://127.0.0.1/v1#fragment", "http://127.0.0.1/other", "file:///v1"} {
		if loopbackURL(endpoint, "", "/v1") {
			t.Errorf("nonlocal or credential-bearing URL accepted: %s", endpoint)
		}
	}
	root := t.TempDir()
	if _, err := reportPath(root, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := reportPath(root, filepath.Join(root, ".local", "lab", "validation-model.json")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "validation.json"), filepath.Join(root, ".local", "lab", "reader.kubeconfig"), filepath.Join(root, ".local", "lab", "state.json"), filepath.Join(root, ".local", "lab", "nested", "validation.json")} {
		if _, err := reportPath(root, path); err == nil {
			t.Errorf("report could overwrite non-report destination: %s", path)
		}
	}
}

func capturedFixture() *core.Investigation {
	pod := core.Resource{Ref: core.ResourceRef{Kind: "pods", Namespace: labNamespace, Name: "checkout-abc-pod", UID: "uid-pod"}, Status: "CrashLoopBackOff", Details: map[string]any{
		"owners":     []any{map[string]any{"kind": "ReplicaSet", "name": "checkout-abc", "uid": "uid-rs", "controller": true}},
		"containers": []any{map[string]any{"name": "checkout", "last_state": map[string]any{"terminated": map[string]any{"exit_code": 1}}}},
	}}
	rs := core.Resource{Ref: core.ResourceRef{Kind: "replicasets", Namespace: labNamespace, Name: "checkout-abc", UID: "uid-rs"}, Details: map[string]any{
		"owners": []any{map[string]any{"kind": "Deployment", "name": "checkout", "uid": "uid-deployment", "controller": true}},
	}}
	deployment := core.Resource{Ref: core.ResourceRef{Kind: "deployments", Namespace: labNamespace, Name: "checkout", UID: "uid-deployment"}}
	i := core.NewInvestigation(labContext, labNamespace, pod.Ref)
	for _, resource := range []core.Resource{pod, rs, deployment} {
		payload, _ := json.Marshal(resource)
		i.AddObservation(core.Observation{Source: "kubernetes/" + resource.Ref.Kind, Ref: resource.Ref, State: "captured", Format: "json", Payload: string(payload)})
	}
	for _, previous := range []string{"true", "false"} {
		i.AddObservation(core.Observation{Source: "kubernetes/pods/log", Ref: pod.Ref, State: "sampled", Format: "log", Query: "previous=" + previous, Payload: "starting\n" + expectedFailure})
	}
	i.AddObservation(core.Observation{Source: "kubernetes/events", Ref: pod.Ref, State: "sampled", Format: "json", Payload: `{"selector":"involvedObject.uid=uid-pod","events":[{"ref":{"namespace":"kubix-lab"},"details":{"involved_object":{"uid":"uid-pod"}}}]}`})
	i.AddObservation(core.Observation{Source: "kubernetes/pods", Ref: pod.Ref, State: "captured", Format: "json", Payload: `{"expected_uid":"uid-pod","observed_uid":"uid-pod","container_changed":false,"container_verified":true}`})
	i.FinishedAt = time.Now().UTC()
	return i
}

func TestCaptureRequiresExactOwnersLogsAndVerifiedIdentity(t *testing.T) {
	if err := validateCapture(capturedFixture()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*core.Investigation){
		"different RS": func(i *core.Investigation) {
			i.Observations[1].Payload = strings.ReplaceAll(i.Observations[1].Payload, "uid-rs", "another-rs")
		},
		"different deployment": func(i *core.Investigation) {
			i.Observations[2].Payload = strings.ReplaceAll(i.Observations[2].Payload, "uid-deployment", "another-deployment")
		},
		"wrong event": func(i *core.Investigation) {
			i.Observations[5].Payload = strings.ReplaceAll(i.Observations[5].Payload, `"uid":"uid-pod"`, `"uid":"other"`)
		},
		"changed logs": func(i *core.Investigation) { i.Observations[3].State = "changed"; i.Observations[3].Payload = "" },
		"unverified container": func(i *core.Investigation) {
			i.Observations[6].Payload = strings.ReplaceAll(i.Observations[6].Payload, `"container_verified":true`, `"container_verified":false`)
		},
		"healthy pod": func(i *core.Investigation) {
			i.Observations[0].Payload = strings.ReplaceAll(i.Observations[0].Payload, "CrashLoopBackOff", "Running")
		},
	} {
		t.Run(name, func(t *testing.T) {
			i := capturedFixture()
			mutate(i)
			if err := validateCapture(i); err == nil {
				t.Fatal("incomplete or inconsistent capture passed")
			}
		})
	}
}

func TestModelFixtureClaimMustCiteItsActualSource(t *testing.T) {
	i := capturedFixture()
	c, err := i.AddCitation(core.EvidenceCitation{ObservationID: "O4", LineStart: 2, LineEnd: 2})
	if err != nil {
		t.Fatal(err)
	}
	answer := core.Answer{Text: "Previous logs report DATABASE_URL is missing [" + c.ID + "].", Investigation: i}
	if got, err := validateAnswer(answer); err != nil || got != c.ID {
		t.Fatalf("exact reference: %q %v", got, err)
	}
	raw := `{"observations":[{"text":"DATABASE_URL is missing in the startup error.","citations":["` + c.ID + `"]}]}`
	if !modelCitesKnownFailure(raw, i) {
		t.Fatal("known supported fixture claim rejected")
	}
	if modelCitesKnownFailure(strings.ReplaceAll(raw, c.ID, "E1"), i) {
		t.Fatal("pod identity was treated as proof of the missing setting")
	}
	if modelCitesKnownFailure(strings.ReplaceAll(raw, "missing", "configured"), i) {
		t.Fatal("wrong fixture diagnosis passed")
	}
	answer.Text = "The source is [E999]."
	if _, err := validateAnswer(answer); err == nil {
		t.Fatal("invented citation passed")
	}
	i.Observations[3].State = "changed"
	if modelCitesKnownFailure(raw, i) {
		t.Fatal("unverified log source was accepted")
	}
}

func TestReportContainsHashesNotRawCapturedPayloads(t *testing.T) {
	i := capturedFixture()
	i.Observations[0].Payload = "sensitive-sentinel-body"
	r := validationReport{SchemaVersion: 1, Sources: sourceSummaries(i), Model: modelSummary{Status: "not_requested"}}
	path := filepath.Join(t.TempDir(), "validation.json")
	if err := writeReport(path, r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sensitive-sentinel-body") || !strings.Contains(string(data), "payload_sha256") || !json.Valid(data) {
		t.Fatal("report leaked captured payload or lost validation metadata")
	}
}

func TestModelDiagnosticsAreBoundedAndExcludePromptAndToolArguments(t *testing.T) {
	ids := transmittedIDs([]agent.Message{{Role: "system", Content: "system-private-prompt"}, {Role: "user", Content: "Question: private-question\n\nCaptured evidence:\n" + `{"evidence":[{"id":"E31","payload":"private-evidence"},{"id":"E2"},{"id":"not-a-reference"}]}`}})
	if len(ids) != 2 || ids[0] != "E31" || ids[1] != "E2" {
		t.Fatalf("incorrect transmitted ID metadata: %v", ids)
	}
	m := observedModel{citationIDs: ids, response: agent.Message{
		Role: "assistant", Content: "password=diagnostic-secret\n" + strings.Repeat("x", 9000),
		ToolCalls: []agent.ToolCall{{Function: agent.FunctionCall{Name: "invented", Arguments: "private-tool-arguments"}}},
	}}
	diagnostic := m.diagnostic([]string{"Model summary rejected: invalid summary"})
	encoded, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"diagnostic-secret", "private-evidence", "private-question", "system-private-prompt", "private-tool-arguments"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("diagnostics included %q", forbidden)
		}
	}
	if !diagnostic.ResponseDisplayBounded || diagnostic.ReturnedToolCalls != 1 || len(diagnostic.ResponseContent) > 8050 || !strings.Contains(string(encoded), "Model summary rejected") {
		t.Fatal("diagnostic metadata/bounds were lost")
	}
}

type structuredFixtureModel struct {
	agent.Model
	schema   json.RawMessage
	messages []agent.Message
}

func (m *structuredFixtureModel) CompleteStructured(_ context.Context, messages []agent.Message, schema json.RawMessage) (agent.Message, error) {
	m.schema = append(json.RawMessage(nil), schema...)
	m.messages = append([]agent.Message(nil), messages...)
	return agent.Message{Role: "assistant", Content: `{"observations":[]}`}, nil
}

func TestObservedModelPreservesStructuredCapabilityWithoutTools(t *testing.T) {
	inner := &structuredFixtureModel{}
	tracked := &observedModel{Model: inner}
	schema := json.RawMessage(`{"type":"object","properties":{"observations":{"type":"array"}}}`)
	messages := []agent.Message{{Role: "user", Content: "Question: fixture\n\nCaptured evidence:\n" + `{"evidence":[{"id":"E1"}]}`}}
	var capability agent.StructuredModel = tracked
	response, err := capability.CompleteStructured(context.Background(), messages, schema)
	if err != nil || response.Role != "assistant" || string(inner.schema) != string(schema) || len(inner.messages) != 1 || inner.messages[0].Content != messages[0].Content {
		t.Fatal("structured adapter dropped or changed the schema/messages")
	}
	diagnostic := tracked.diagnostic(nil)
	if tracked.calls != 1 || tracked.tools != 0 || tracked.failed || !diagnostic.StructuredRequest || len(diagnostic.TransmittedCitationIDs) != 1 || diagnostic.TransmittedCitationIDs[0] != "E1" {
		t.Fatal("structured request lost diagnostics or exposed tools")
	}
}
