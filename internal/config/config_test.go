package config

import (
	"strings"
	"testing"
)

func TestOfflineDemoIgnoresEnvironment(t *testing.T) {
	t.Setenv("KUBIX_MODEL_BASE_URL", "invalid://server")
	t.Setenv("KUBIX_MODEL_API_KEY", "secret")
	s, err := FromEnv("", "", true)
	if err != nil || s.BaseURL != "" || s.APIKey != "" {
		t.Fatalf("demo must be offline: %v %v", s, err)
	}
}

func TestExplicitDemoEndpoint(t *testing.T) {
	t.Setenv("KUBIX_MODEL_API_KEY", "secret")
	s, err := FromEnv("http://localhost:8000/v1/", "qwen", true)
	if err != nil || s.BaseURL != "http://localhost:8000/v1" || s.Model != "qwen" || s.APIKey != "secret" {
		t.Fatalf("unexpected settings: %v %v", s, err)
	}
	if strings.Contains(s.String(), "secret") {
		t.Fatal("settings leak API key")
	}
}

func TestInvalidModelURLs(t *testing.T) {
	for _, url := range []string{"file:///etc/passwd", "http://user:password@model/v1", "http://model/v1?token=x", "http://model/v1#fragment"} {
		s := DefaultModelSettings()
		s.BaseURL = url
		if s.Validate() == nil {
			t.Fatalf("accepted unsafe URL %s", url)
		}
	}
}
