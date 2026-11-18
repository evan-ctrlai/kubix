package config

import (
	"errors"
	"net/url"
	"os"
	"strings"
	"time"
)

type ModelSettings struct {
	BaseURL              string
	Model                string
	APIKey               string
	Timeout              time.Duration
	InvestigationTimeout time.Duration
	MaxToolCalls         int
	MaxModelCalls        int
}

func DefaultModelSettings() ModelSettings {
	return ModelSettings{Model: "local-model", Timeout: 30 * time.Second, InvestigationTimeout: 90 * time.Second, MaxToolCalls: 8, MaxModelCalls: 8}
}

// FromEnv preserves offline demo mode unless the endpoint is explicitly provided.
func FromEnv(baseURL, model string, demo bool) (ModelSettings, error) {
	s := DefaultModelSettings()
	if demo && baseURL == "" {
		return s, nil
	}
	if baseURL == "" {
		baseURL = os.Getenv("KUBIX_MODEL_BASE_URL")
	}
	if model == "" {
		model = os.Getenv("KUBIX_MODEL_NAME")
	}
	s.BaseURL = strings.TrimRight(baseURL, "/")
	if model != "" {
		s.Model = model
	}
	s.APIKey = os.Getenv("KUBIX_MODEL_API_KEY")
	return s, s.Validate()
}

func (s ModelSettings) Validate() error {
	if s.BaseURL != "" {
		u, err := url.Parse(s.BaseURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("model URL must be an HTTP(S) endpoint, such as http://localhost:8000/v1")
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return errors.New("model URL cannot contain credentials, a query, or a fragment")
		}
	}
	if s.Timeout <= 0 || s.InvestigationTimeout < s.Timeout {
		return errors.New("model timeout must be positive and within the investigation budget")
	}
	if s.MaxToolCalls < 1 || s.MaxModelCalls < 1 {
		return errors.New("investigation budgets must be positive")
	}
	return nil
}

// String avoids accidentally logging the API key when formatting settings.
func (s ModelSettings) String() string {
	return "ModelSettings{model=" + s.Model + ", endpoint=" + s.BaseURL + ", api_key=[REDACTED]}"
}
