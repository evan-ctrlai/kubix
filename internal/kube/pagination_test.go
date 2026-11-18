package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"kubix/internal/core"
)

func TestExplicitPaginationBeyondFirstFifty(t *testing.T) {
	api := fake.NewClientset()
	calls := 0
	api.PrependReactor("list", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		calls++
		opts := a.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		if a.GetNamespace() != "apps" || opts.Limit != 50 {
			t.Fatalf("bad scope/page: %v %+v", a, opts)
		}
		start := 0
		next := "opaque-next/+=token"
		if opts.Continue != "" {
			if opts.Continue != next {
				t.Fatalf("cursor altered: %q", opts.Continue)
			}
			start = 50
			next = ""
		}
		items := []corev1.Pod{}
		for n := start; n < start+50; n++ {
			items = append(items, *testPod(fmt.Sprintf("pod-%03d", n), "apps"))
		}
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: next}, Items: items}, nil
	})
	b := NewWithClient(api, "test", "apps")
	first := b.ListResourcesPage(context.Background(), "pods", "", "", 50, "")
	if calls != 1 || len(first.Result.Resources) != 50 || first.NextCursor == "" || first.NextCursor == "opaque-next/+=token" || first.Result.Data["truncated"] != true {
		t.Fatalf("page1: %+v calls%d", first, calls)
	}
	second := b.ListResourcesPage(context.Background(), "pods", "apps", "", 50, first.NextCursor)
	if calls != 2 || len(second.Result.Resources) != 50 || second.Result.Resources[0].Ref.Name != "pod-050" || second.NextCursor != "" || second.Result.Data["truncated"] != false || second.Result.Data["limited"] != false {
		t.Fatalf("page2: %+v calls%d", second, calls)
	}
	// Legacy model callers still make exactly one first-page read, with no cursor.
	legacy := b.ListResources(context.Background(), "pods", "apps", "", 50)
	encoded, _ := json.Marshal(legacy)
	if calls != 3 || strings.Contains(string(encoded), "opaque-next") {
		t.Fatalf("legacy leaked/scanned %s calls%d", encoded, calls)
	}
}

func TestFilteredEmptyPageStillHasContinuation(t *testing.T) {
	api := fake.NewClientset()
	api.PrependReactor("list", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next"}, Items: []corev1.Pod{*testPod("unrelated", "apps")}}, nil
	})
	page := NewWithClient(api, "test", "apps").ListResourcesPage(context.Background(), "pods", "apps", "checkout", 1, "")
	if len(page.Result.Resources) != 0 || page.NextCursor == "" || page.Result.Data["truncated"] != true || len(api.Actions()) != 1 {
		t.Fatal(page)
	}
}

func TestCursorRejectsChangedViewSessionAndOversizeBeforeRead(t *testing.T) {
	api := fake.NewClientset()
	b := NewWithClient(api, "same-name", "")
	cursor, err := b.encodeResourceCursor("pods", "apps", "", 50, "next")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind, ns, q string
		limit       int
		cursor      string
	}{{"services", "apps", "", 50, cursor}, {"pods", "other", "", 50, cursor}, {"pods", "apps", "query", 50, cursor}, {"pods", "apps", "", 100, cursor}, {"pods", "apps", "", 50, strings.Repeat("a", core.MaxResourceCursorBytes+1)}, {"pods", "apps", "", 50, "not+base64"}, {"pods", "apps", "", 101, ""}} {
		p := b.ListResourcesPage(context.Background(), tc.kind, tc.ns, tc.q, tc.limit, tc.cursor)
		if len(p.Result.Errors) == 0 {
			t.Fatalf("accepted %+v", tc)
		}
	}
	other := NewWithClient(api, "same-name", "")
	p := other.ListResourcesPage(context.Background(), "pods", "apps", "", 50, cursor)
	if len(p.Result.Errors) == 0 {
		t.Fatal("cross-backend cursor accepted")
	}
	if len(api.Actions()) != 0 {
		t.Fatal("invalid cursor made API calls")
	}
}

func TestMalformedCursorEnvelopeRejected(t *testing.T) {
	api := fake.NewClientset()
	b := NewWithClient(api, "test", "apps")
	for _, raw := range []string{`{"v":1,"c":"next","extra":true}`, `{} {}`, `null`, `{"v":2,"c":"next"}`} {
		p := b.ListResourcesPage(context.Background(), "pods", "apps", "", 50, base64.RawURLEncoding.EncodeToString([]byte(raw)))
		if len(p.Result.Errors) == 0 {
			t.Fatal("accepted malformed cursor")
		}
	}
	if len(api.Actions()) != 0 {
		t.Fatal(api.Actions())
	}
}

func TestExpiredContinuationDoesNotRestartOrScan(t *testing.T) {
	api := fake.NewClientset()
	b := NewWithClient(api, "test", "apps")
	cursor, _ := b.encodeResourceCursor("pods", "apps", "", 50, "expired")
	api.PrependReactor("list", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewResourceExpired("private server detail")
	})
	p := b.ListResourcesPage(context.Background(), "pods", "apps", "", 50, cursor)
	if len(api.Actions()) != 1 || p.NextCursor != "" || !strings.Contains(strings.Join(p.Result.Errors, " "), "410") || strings.Contains(strings.Join(p.Result.Errors, " "), "private") {
		t.Fatal(p)
	}
}

func TestPageCancellationAndOversizeServerCursor(t *testing.T) {
	api := fake.NewClientset()
	b := NewWithClient(api, "test", "apps")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := b.ListResourcesPage(ctx, "pods", "apps", "", 50, "")
	if len(p.Result.Errors) == 0 || len(api.Actions()) != 0 {
		t.Fatal(p)
	}
	api.PrependReactor("list", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: strings.Repeat("x", maxServerCursorBytes+1)}, Items: []corev1.Pod{*testPod("one", "apps")}}, nil
	})
	p = b.ListResourcesPage(context.Background(), "pods", "apps", "", 50, "")
	if len(p.Result.Resources) != 1 || len(p.Result.Errors) == 0 || p.NextCursor != "" || p.Result.Data["truncated"] != true {
		t.Fatal(p)
	}
}

func TestLegacyListDoesNotDependOnUICursorBudget(t *testing.T) {
	api := fake.NewClientset()
	api.PrependReactor("list", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: strings.Repeat("x", maxServerCursorBytes+1)}, Items: []corev1.Pod{*testPod("one", "apps")}}, nil
	})
	b := NewWithClient(api, "test", "apps")
	result := b.ListResources(context.Background(), "pods", "apps", "", 50)
	if len(result.Errors) != 0 || len(result.Resources) != 1 || result.Data["truncated"] != true || len(api.Actions()) != 1 {
		t.Fatal(result)
	}
}
