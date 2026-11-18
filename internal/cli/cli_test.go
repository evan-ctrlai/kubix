package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestFlags(t *testing.T) {
	var out bytes.Buffer
	o, err := Parse([]string{"--demo"}, &out)
	if err != nil || !o.Demo || o.Namespace != "default" {
		t.Fatal(o, err)
	}
	o, err = Parse([]string{"-A", "--context", "prod"}, &out)
	if err != nil || o.Namespace != "" || o.Context != "prod" {
		t.Fatal(o, err)
	}
	if _, err = Parse([]string{"-A", "-n", "demo"}, &out); err == nil {
		t.Fatal("accepted conflicting scope")
	}
	if _, err = Parse([]string{"unexpected"}, &out); err == nil {
		t.Fatal("accepted positional argument")
	}
	for _, args := range [][]string{{"--namespace", ""}, {"--namespace="}, {"-n", " "}} {
		if _, err = Parse(args, &out); err == nil {
			t.Fatalf("empty namespace expanded scope: %q", args)
		}
	}
}

func TestLocalModelTimeoutFlags(t *testing.T) {
	var out bytes.Buffer
	opts, err := Parse([]string{"--model-timeout", "300s", "--investigation-timeout", "360s"}, &out)
	if err != nil || opts.ModelTimeout != 5*time.Minute || opts.InvestigationTimeout != 6*time.Minute {
		t.Fatalf("local inference deadlines not preserved: %+v %v", opts, err)
	}
	for _, args := range [][]string{
		{"--model-timeout", "0s"},
		{"--model-timeout", "-1s"},
		{"--model-timeout", "300s"},
		{"--investigation-timeout", "0s"},
		{"--model-timeout", "invalid"},
	} {
		if Run(args, &out, &out) != 2 {
			t.Fatalf("invalid deadlines were not rejected before cluster startup: %v", args)
		}
	}
}

func TestVersionAndHelp(t *testing.T) {
	var out, errout bytes.Buffer
	if Run([]string{"--version"}, &out, &errout) != 0 || !strings.Contains(out.String(), "(Go)") {
		t.Fatal(out.String(), errout.String())
	}
	out.Reset()
	if Run([]string{"--help"}, &out, &errout) != 0 || !strings.Contains(out.String(), "--demo") && !strings.Contains(out.String(), "-demo") {
		t.Fatal(out.String(), errout.String())
	}
}

func TestNamespaceExplicitFlagPreserved(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{nil, {"--demo"}, {"-A"}} {
		o, err := Parse(args, &out)
		if err != nil || o.NamespaceExplicit {
			t.Fatalf("unexpected explicit namespace %+v %v", o, err)
		}
	}
	for _, args := range [][]string{{"-n", "default"}, {"--namespace=apps"}} {
		o, err := Parse(args, &out)
		if err != nil || !o.NamespaceExplicit {
			t.Fatalf("lost explicit namespace %+v %v", o, err)
		}
	}
}
