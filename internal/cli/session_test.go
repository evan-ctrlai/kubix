package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"kubix/internal/config"
)

func TestSessionOpenerIsolatedScopeAndCancellation(t *testing.T) {
	fixture := `apiVersion: v1
kind: Config
current-context: first
clusters:
- name: fake
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: first
  context:
    cluster: fake
    namespace: first-ns
- name: second
  context:
    cluster: fake
    namespace: second-ns
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	opener := sessionOpener(Options{Kubeconfig: path}, config.DefaultModelSettings())
	first, err := opener(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := opener(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	if first.Backend.Namespace() != "first-ns" || second.Backend.Namespace() != "second-ns" {
		t.Fatal("lost selected namespace")
	}
	if first.Backend == second.Backend || first.Investigator == second.Investigator {
		t.Fatal("sessions share mutable investigator or backend")
	}
	for _, i := range []interface{}{first.Investigator, second.Investigator} {
		if c, ok := i.(interface{ Close() }); ok {
			c.Close()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bad, err := opener(ctx, "second")
	if !errors.Is(err, context.Canceled) || bad.Backend != nil || bad.Investigator != nil {
		t.Fatalf("cancelled opener=%+v %v", bad, err)
	}
	if _, err := opener(context.Background(), "missing"); err == nil {
		t.Fatal("unknown context succeeded")
	}
	after, _ := os.ReadFile(path)
	if string(after) != fixture {
		t.Fatal("factory wrote config")
	}
}
