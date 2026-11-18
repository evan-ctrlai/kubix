// Package labtest contains checks used by the opt-in, read-only incident lab.
// Normal go test runs use in-memory fixtures and never contact a cluster/model.
package labtest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"kubix/internal/core"
)

const (
	labContext      = "kubix-lab-reader"
	labNamespace    = "kubix-lab"
	expectedFailure = "FATAL: required setting DATABASE_URL is missing"
)

var referencePattern = regexp.MustCompile(`\[(E[1-9][0-9]*)\]`)
var missingSetting = regexp.MustCompile(`(?i)\b(missing|unset|absent|not (?:set|configured|provided|defined))\b`)

type validationReport struct {
	SchemaVersion                   int              `json:"schema_version"`
	StartedAt                       time.Time        `json:"started_at"`
	FinishedAt                      time.Time        `json:"finished_at"`
	Passed                          bool             `json:"passed"`
	Scenario                        string           `json:"scenario"`
	Context                         string           `json:"context"`
	Namespace                       string           `json:"namespace"`
	Target                          core.ResourceRef `json:"target"`
	CaptureAttempts                 int              `json:"capture_attempts"`
	CaptureDurationMS               int64            `json:"capture_duration_ms"`
	CaptureValidated                bool             `json:"capture_validated"`
	Sources                         []sourceSummary  `json:"sources"`
	ExactFailureCitation            string           `json:"exact_failure_citation,omitempty"`
	KnownFailureExcerpt             string           `json:"known_failure_excerpt,omitempty"`
	DeterministicCitationsValidated bool             `json:"deterministic_citations_validated"`
	Model                           modelSummary     `json:"model"`
	Failure                         string           `json:"failure,omitempty"`
	Limitations                     []string         `json:"limitations"`
}

type sourceSummary struct {
	ID            string           `json:"id"`
	Source        string           `json:"source"`
	Ref           core.ResourceRef `json:"ref"`
	CapturedAt    time.Time        `json:"captured_at"`
	State         string           `json:"state"`
	PayloadBytes  int              `json:"payload_bytes"`
	PayloadSHA256 string           `json:"payload_sha256"`
}

type modelSummary struct {
	Requested              bool             `json:"requested"`
	Name                   string           `json:"name,omitempty"`
	Status                 string           `json:"status"`
	DurationMS             int64            `json:"duration_ms"`
	Requests               int              `json:"requests"`
	ToolsOffered           int              `json:"tools_offered"`
	KnownFailureClaimCited bool             `json:"known_failure_claim_cited"`
	Answer                 string           `json:"answer,omitempty"`
	Diagnostic             *modelDiagnostic `json:"diagnostic,omitempty"`
}

// Diagnostics are lab-only, bounded and redacted. They intentionally exclude
// request bodies, credentials, kubeconfig contents, and returned tool arguments.
type modelDiagnostic struct {
	StructuredRequest      bool     `json:"structured_request"`
	ResponseContent        string   `json:"response_content,omitempty"`
	ResponseBytes          int      `json:"response_bytes"`
	ResponseDisplayBounded bool     `json:"response_display_bounded"`
	ResponseRole           string   `json:"response_role,omitempty"`
	ReturnedToolCalls      int      `json:"returned_tool_calls"`
	RequestError           string   `json:"request_error,omitempty"`
	TransmittedCitationIDs []string `json:"transmitted_citation_ids"`
	Progress               []string `json:"progress"`
}

func validateReaderConfig(cfg *clientcmdapi.Config) error {
	if cfg == nil || cfg.CurrentContext != labContext || len(cfg.Contexts) != 1 || len(cfg.Clusters) != 1 || len(cfg.AuthInfos) != 1 {
		return errors.New("use the lab's dedicated reader kubeconfig with one reader context, cluster, and identity")
	}
	ctx := cfg.Contexts[labContext]
	if ctx == nil || ctx.Namespace != labNamespace {
		return errors.New("reader context must select the kubix-lab namespace")
	}
	cluster, auth := cfg.Clusters[ctx.Cluster], cfg.AuthInfos[ctx.AuthInfo]
	if cluster == nil || auth == nil || !loopbackURL(cluster.Server, "https", "") || cluster.InsecureSkipTLSVerify || len(cluster.CertificateAuthorityData) == 0 || cluster.ProxyURL != "" {
		return errors.New("reader cluster must use a loopback HTTPS API with embedded CA verification and no proxy")
	}
	// A copied administrator certificate or exec auth provider must never be
	// accidentally accepted by the validation path intended for reader tokens.
	identity := *auth
	identity.Token, identity.LocationOfOrigin = "", ""
	// clientcmd's decoder initializes some empty slices/maps. Empty collections
	// carry no authentication behavior and are equivalent to nil. Populated
	// collections and all executable/provider pointers remain rejected.
	if len(identity.ClientCertificateData) == 0 {
		identity.ClientCertificateData = nil
	}
	if len(identity.ClientKeyData) == 0 {
		identity.ClientKeyData = nil
	}
	if len(identity.ImpersonateGroups) == 0 {
		identity.ImpersonateGroups = nil
	}
	if len(identity.ImpersonateUserExtra) == 0 {
		identity.ImpersonateUserExtra = nil
	}
	if len(identity.Extensions) == 0 {
		identity.Extensions = nil
	}
	if auth.Token == "" || !reflect.DeepEqual(identity, clientcmdapi.AuthInfo{}) {
		return errors.New("reader identity must contain only a short-lived service account token")
	}
	parts := strings.Split(auth.Token, ".")
	if len(parts) != 3 || len(auth.Token) > 16000 {
		return errors.New("reader token must be a bounded, expiring service account JWT")
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	var identityClaims struct {
		Subject string `json:"sub"`
		Expires int64  `json:"exp"`
	}
	if err != nil || json.Unmarshal(claims, &identityClaims) != nil || identityClaims.Subject != "system:serviceaccount:kubix-lab:kubix-reader" || identityClaims.Expires <= time.Now().Unix() {
		return errors.New("reader token must identify kubix-reader in kubix-lab and must not be expired; run lab up to refresh it")
	}
	// Claims are checked to catch accidental credentials, not to authenticate
	// the token. The Kubernetes API still verifies its signature and permissions.
	return nil
}

func loopbackURL(raw, scheme, path string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != path && u.Path != path+"/") {
		return false
	}
	if scheme != "" && u.Scheme != scheme {
		return false
	}
	if scheme == "" && u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func reportPath(repoRoot, requested string) (string, error) {
	root, err := filepath.Abs(filepath.Join(repoRoot, ".local", "lab"))
	if err != nil {
		return "", errors.New("cannot resolve lab report directory")
	}
	if requested == "" {
		requested = filepath.Join(root, "validation.json")
	}
	path, err := filepath.Abs(requested)
	if err != nil || filepath.Clean(filepath.Dir(path)) != filepath.Clean(root) || (filepath.Base(path) != "validation.json" && !(strings.HasPrefix(filepath.Base(path), "validation-") && filepath.Ext(path) == ".json")) {
		return "", errors.New("report must be validation.json or validation-*.json directly inside .local/lab")
	}
	return path, nil
}

func sourceSummaries(inv *core.Investigation) []sourceSummary {
	result := []sourceSummary{}
	for _, source := range inv.Observations {
		digest := sha256.Sum256([]byte(source.Payload))
		result = append(result, sourceSummary{source.ID, source.Source, source.Ref, source.CapturedAt, source.State, len(source.Payload), hex.EncodeToString(digest[:])})
	}
	return result
}

// validateCapture verifies the known lab fixture against the actual collector's
// retained sources, independently checking the owner chain and event identities.
func validateCapture(inv *core.Investigation) error {
	if inv == nil || inv.Context != labContext || inv.Namespace != labNamespace || inv.Target.Kind != "pods" || inv.Target.Namespace != labNamespace || inv.Target.UID == "" || !strings.HasPrefix(inv.Target.Name, "checkout-") {
		return errors.New("capture did not identify the expected lab pod")
	}
	if inv.StartedAt.IsZero() || inv.FinishedAt.Before(inv.StartedAt) {
		return errors.New("capture lacks valid start/end times")
	}
	resources := map[string]core.Resource{}
	previous, current, identity, events := false, false, false, false
	for _, o := range inv.Observations {
		if o.CapturedAt.IsZero() || o.Ref.Namespace != labNamespace {
			return errors.New("capture source lacks timestamp or escaped the lab namespace")
		}
		if o.State != "captured" && o.State != "sampled" && o.State != "changed" {
			continue
		}
		if o.Source == "kubernetes/pods/log" && (o.State == "captured" || o.State == "sampled") && o.Ref == inv.Target && strings.Contains(o.Payload, expectedFailure) {
			previous = previous || strings.Contains(o.Query, "previous=true")
			current = current || strings.Contains(o.Query, "previous=false")
		}
		if o.Source == "kubernetes/pods" {
			if verified, ok := core.JSONPointerValue(o.Payload, "/container_verified"); ok && verified == true {
				initial, _ := core.JSONPointerValue(o.Payload, "/expected_uid")
				final, _ := core.JSONPointerValue(o.Payload, "/observed_uid")
				changed, _ := core.JSONPointerValue(o.Payload, "/container_changed")
				identity = initial == inv.Target.UID && final == inv.Target.UID && changed == false
			}
		}
		if o.Source == "kubernetes/events" {
			var page struct {
				Events   []core.Resource `json:"events"`
				Selector string          `json:"selector"`
			}
			if json.Unmarshal([]byte(o.Payload), &page) != nil || page.Selector != "involvedObject.uid="+inv.Target.UID {
				return errors.New("event page is not tied to the exact pod UID")
			}
			for _, event := range page.Events {
				involved, _ := event.Details["involved_object"].(map[string]any)
				if event.Ref.Namespace != labNamespace || involved["uid"] != inv.Target.UID {
					return errors.New("event page contains a different pod identity")
				}
			}
			events = true // An empty retained event page remains a valid sample.
		}
		var resource core.Resource
		if o.Format == "json" && json.Unmarshal([]byte(o.Payload), &resource) == nil && resource.Ref.UID != "" && resource.Ref == o.Ref {
			resources[resource.Ref.Kind] = resource
		}
	}
	pod, podOK := resources["pods"]
	rs, rsOK := resources["replicasets"]
	deployment, deploymentOK := resources["deployments"]
	if !podOK || pod.Ref != inv.Target || pod.Status != "CrashLoopBackOff" || !rsOK || !deploymentOK || deployment.Ref.Name != "checkout" {
		return errors.New("capture lacks the CrashLoop pod and its ReplicaSet/Deployment")
	}
	if !controllerMatches(pod, rs, "ReplicaSet") || !controllerMatches(rs, deployment, "Deployment") {
		return errors.New("captured workload owner UIDs do not match")
	}
	exitOne := false
	if containers, ok := pod.Details["containers"].([]any); ok {
		for _, item := range containers {
			container, _ := item.(map[string]any)
			if container["name"] != "checkout" {
				continue
			}
			last, _ := container["last_state"].(map[string]any)
			termination, _ := last["terminated"].(map[string]any)
			exitOne = termination["exit_code"] == float64(1)
		}
	}
	if !exitOne || !previous || !current || !identity || !events {
		return fmt.Errorf("capture incomplete: exit1=%t previous_error=%t current_error=%t verified_identity=%t event_sample=%t", exitOne, previous, current, identity, events)
	}
	return nil
}

func controllerMatches(child, owner core.Resource, kind string) bool {
	owners, _ := child.Details["owners"].([]any)
	count, matched := 0, false
	for _, item := range owners {
		ref, _ := item.(map[string]any)
		if ref["controller"] == true {
			count++
			matched = ref["kind"] == kind && ref["name"] == owner.Ref.Name && ref["uid"] == owner.Ref.UID && child.Ref.Namespace == owner.Ref.Namespace
		}
	}
	return count == 1 && matched
}

func validateAnswer(answer core.Answer) (string, error) {
	if answer.Investigation == nil {
		return "", errors.New("agent answer did not retain its capture")
	}
	refs := referencePattern.FindAllStringSubmatch(answer.Text, -1)
	if len(refs) == 0 {
		return "", errors.New("answer has no captured citations")
	}
	failureCitation := ""
	for _, match := range refs {
		o, c, ok := answer.Investigation.ResolveCitation(match[1])
		if !ok {
			return "", errors.New("answer contains an unresolved captured citation")
		}
		if c.LineStart > 0 && o.Source == "kubernetes/pods/log" && strings.Contains(o.Query, "previous=true") && strings.Contains(citationText(o, c), expectedFailure) {
			failureCitation = c.ID
		}
	}
	if failureCitation == "" {
		return "", errors.New("answer did not cite the exact previous-log failure line")
	}
	return failureCitation, nil
}

func citationText(o core.Observation, c core.EvidenceCitation) string {
	if c.LineStart > 0 {
		lines := strings.Split(strings.TrimSuffix(o.Payload, "\n"), "\n")
		if c.LineStart < 1 || c.LineEnd < c.LineStart || c.LineEnd > len(lines) {
			return ""
		}
		return strings.Join(lines[c.LineStart-1:c.LineEnd], "\n")
	}
	if c.JSONPointer != "" {
		value, ok := core.JSONPointerValue(o.Payload, c.JSONPointer)
		if !ok {
			return ""
		}
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	return o.Payload
}

// This is a narrow fixture assertion, not a general diagnosis-quality metric.
// Require the model's missing-setting claim to cite the actual failure source.
func modelCitesKnownFailure(raw string, inv *core.Investigation) bool {
	var result struct {
		Observations []struct {
			Text      string   `json:"text"`
			Citations []string `json:"citations"`
		} `json:"observations"`
		Hypotheses []struct {
			Text      string   `json:"text"`
			Citations []string `json:"citations"`
		} `json:"hypotheses"`
	}
	if json.Unmarshal([]byte(raw), &result) != nil {
		return false
	}
	for _, claim := range append(result.Observations, result.Hypotheses...) {
		if !strings.Contains(strings.ToUpper(claim.Text), "DATABASE_URL") || !missingSetting.MatchString(claim.Text) {
			continue
		}
		for _, id := range claim.Citations {
			o, c, ok := inv.ResolveCitation(id)
			if ok && (o.State == "sampled" || o.State == "captured") && o.Source == "kubernetes/pods/log" && strings.Contains(citationText(o, c), expectedFailure) {
				return true
			}
		}
	}
	return false
}
