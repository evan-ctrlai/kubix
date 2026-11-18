package kube

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const contextFixture = `apiVersion: v1
kind: Config
current-context: beta
clusters:
- name: fake
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: beta
  context:
    cluster: fake
    user: no-auth
    namespace: team-b
- name: alpha
  context:
    cluster: fake
    user: no-auth
users:
- name: no-auth
  user: {}
`

func writeContextFixture(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestContextMetadataDiscoveryDoesNotResolveAuthOrContactCluster(t *testing.T) {
	// The intentionally nonexistent exec plugin would fail if discovery executed
	// authentication. No server is listening at the fixture endpoint.
	data := contextFixture[:len(contextFixture)-len("  user: {}\n")] + `  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kubix-nonexistent-auth-plugin
      interactiveMode: Never
`
	path := writeContextFixture(t, data)
	got, err := ListContexts(path)
	want := []ContextChoice{{Name: "alpha", Namespace: "default"}, {Name: "beta", Namespace: "team-b", Current: true}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("choices=%+v err=%v", got, err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != data {
		t.Fatal("discovery changed kubeconfig")
	}
}

func TestScopedContextNamespaceResolutionAndConfigImmutability(t *testing.T) {
	path := writeContextFixture(t, contextFixture)
	cases := []struct {
		name, context string
		scope         ScopeOptions
		want          string
	}{
		{"current context", "", ScopeOptions{}, "team-b"},
		{"selected default", "alpha", ScopeOptions{}, "default"},
		{"selected namespace", "beta", ScopeOptions{}, "team-b"},
		{"explicit override", "beta", ScopeOptions{Namespace: "override", NamespaceExplicit: true}, "override"},
		{"all namespaces", "beta", ScopeOptions{AllNamespaces: true}, ""},
		{"ignore implicit cli default", "beta", ScopeOptions{Namespace: "default"}, "team-b"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			b, err := NewScoped(tt.context, path, tt.scope)
			if err != nil {
				t.Fatal(err)
			}
			if b.Namespace() != tt.want {
				t.Fatalf("scope=%q want=%q", b.Namespace(), tt.want)
			}
			selected := tt.context
			if selected == "" {
				selected = "beta"
			}
			if b.ContextName() != selected {
				t.Fatalf("context=%q", b.ContextName())
			}
		})
	}
	after, _ := os.ReadFile(path)
	if string(after) != contextFixture {
		t.Fatal("session creation changed kubeconfig")
	}
}

func TestScopedContextErrorsDoNotFallback(t *testing.T) {
	path := writeContextFixture(t, contextFixture)
	for _, scope := range []ScopeOptions{
		{NamespaceExplicit: true},
		{NamespaceExplicit: true, Namespace: "bad/ns"},
		{NamespaceExplicit: true, Namespace: "team-b", AllNamespaces: true},
	} {
		if _, err := NewScoped("beta", path, scope); err == nil {
			t.Fatalf("accepted %+v", scope)
		}
	}
	if _, err := NewScoped("missing", path, ScopeOptions{}); err == nil {
		t.Fatal("missing context silently fell back")
	}
	if _, err := ListContexts(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing explicit config ignored")
	}
	// The compatibility constructor's empty namespace still means all namespaces.
	b, err := New("beta", "", path)
	if err != nil || b.Namespace() != "" {
		t.Fatalf("legacy New changed: %v", err)
	}
}
