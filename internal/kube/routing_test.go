package kube

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"testing"
)

func TestServicePodSelectorBeforePageLimit(t *testing.T) {
	objects := []runtime.Object{}
	for j := 0; j < 65; j++ {
		app := "unrelated"
		if j >= 60 {
			app = "target"
		}
		objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%03d", j), Namespace: "demo", UID: types.UID(fmt.Sprintf("uid-%d", j)), Labels: map[string]string{"app": app}}})
	}
	client := fake.NewClientset(objects...)
	b := NewWithClient(client, "test", "demo")
	r := b.ListByLabels(context.Background(), "pods", "demo", map[string]string{"app": "target"}, 30)
	if len(r.Errors) > 0 || len(r.Resources) != 5 {
		t.Fatalf("%+v", r)
	}
	a := client.Actions()[0].(ktesting.ListAction)
	if a.GetListRestrictions().Labels.String() != "app=target" {
		t.Fatal(a)
	}
	for _, p := range r.Resources {
		if p.Ref.Name < "pod-060" {
			t.Fatal(p.Ref)
		}
	}
}
func TestServiceSelectorContinuationIsExplicit(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next-page"}}, nil
	})
	r := NewWithClient(client, "test", "demo").ListByLabels(context.Background(), "pods", "demo", map[string]string{"app": "target"}, 30)
	if r.Data["truncated"] != true || r.Data["limited"] != true {
		t.Fatal(r)
	}
}
func TestServiceSelectorRejectsUnboundedAndOutOfScope(t *testing.T) {
	client := fake.NewClientset()
	b := NewWithClient(client, "test", "demo")
	for _, r := range []struct {
		ns       string
		selector map[string]string
		limit    int
	}{{"other", map[string]string{"app": "a"}, 30}, {"demo", nil, 30}, {"demo", map[string]string{"app": "a"}, 101}} {
		got := b.ListByLabels(context.Background(), "pods", r.ns, r.selector, r.limit)
		if len(got.Errors) == 0 {
			t.Fatal(got)
		}
	}
	if len(client.Actions()) != 0 {
		t.Fatal(client.Actions())
	}
}
