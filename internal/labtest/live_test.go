package labtest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"kubix/internal/agent"
	"kubix/internal/config"
	"kubix/internal/core"
	"kubix/internal/kube"
	"kubix/internal/redact"
)

// Run only after labs/crashloop's setup and RBAC verification. No cluster
// mutation is implemented here, and no model/cluster access occurs by default.
func TestLiveCrashLoop(t *testing.T) {
	if os.Getenv("KUBIX_LAB_RUN") != "1" {
		t.Skip("opt-in lab: set KUBIX_LAB_RUN=1 through the lab verify command")
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	path, err := reportPath(root, os.Getenv("KUBIX_LAB_REPORT"))
	if err != nil {
		t.Fatal(err)
	}
	report := validationReport{
		SchemaVersion: 1, StartedAt: time.Now().UTC(), Scenario: "checkout missing DATABASE_URL",
		Context: labContext, Namespace: labNamespace,
		Model: modelSummary{Status: "not_requested"},
		Limitations: []string{
			"Controlled local incident, not a production incident or a general model benchmark.",
			"Capture timing includes bounded discovery/retries; inference timing is reported separately.",
			"Model validation checks accepted citation structure and a cited known fixture claim, not all semantic correctness.",
			"RBAC is verified separately by the lab scripts using the same reader identity.",
			"Model inference reuses the verified capture; it does not issue fresh cluster reads during inference.",
		},
	}
	t.Cleanup(func() {
		report.FinishedAt, report.Passed = time.Now().UTC(), !t.Failed()
		if err := writeReport(path, report); err != nil {
			t.Errorf("write validation report: %v", err)
		}
	})
	fail := func(message string) { t.Helper(); report.Failure = message; t.Fatal(message) }
	for name, expected := range map[string]string{"KUBIX_LAB_CONTEXT": labContext, "KUBIX_LAB_NAMESPACE": labNamespace} {
		if value := os.Getenv(name); value != "" && value != expected {
			fail("lab context/namespace overrides must match the dedicated lab")
		}
	}
	kubeconfig := os.Getenv("KUBIX_LAB_KUBECONFIG")
	if kubeconfig == "" || !filepath.IsAbs(kubeconfig) {
		fail("KUBIX_LAB_KUBECONFIG must be the absolute path to the lab reader kubeconfig")
	}
	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		fail("cannot read the dedicated lab reader kubeconfig; run lab up first")
	}
	if err := validateReaderConfig(cfg); err != nil {
		fail(err.Error())
	}
	modelURL, modelName := os.Getenv("KUBIX_LAB_MODEL_URL"), os.Getenv("KUBIX_LAB_MODEL")
	if (modelURL == "") != (modelName == "") {
		fail("set both KUBIX_LAB_MODEL_URL and KUBIX_LAB_MODEL, or neither")
	}
	if modelURL != "" {
		if !loopbackURL(modelURL, "", "/v1") || len(modelName) > 100 || strings.ContainsAny(modelName, "\r\n\x1b") || strings.Contains(strings.ToLower(modelName), "cloud") {
			fail("model validation requires a loopback /v1 endpoint and a local model name")
		}
		report.Model.Requested, report.Model.Name, report.Model.Status = true, modelName, "not_attempted"
	}
	backend, err := kube.New(labContext, labNamespace, kubeconfig)
	if err != nil {
		fail("cannot initialize the lab reader client")
	}
	start := time.Now()
	captureCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var capture *core.Investigation
	var lastError error
	for attempt := 1; attempt <= 4 && captureCtx.Err() == nil; attempt++ {
		report.CaptureAttempts = attempt
		listed := backend.ListResources(captureCtx, "pods", labNamespace, "checkout-", 100)
		candidates := []core.Resource{}
		for _, resource := range listed.Resources {
			if strings.HasPrefix(resource.Ref.Name, "checkout-") && resource.Status == "CrashLoopBackOff" {
				candidates = append(candidates, resource)
			}
		}
		if len(listed.Errors) > 0 {
			lastError = errors.New("reader could not list lab pods; check token expiration, connectivity, and RBAC")
		} else if len(candidates) != 1 {
			lastError = errors.New("expected exactly one checkout pod in CrashLoopBackOff; run the lab reset and wait steps")
		} else {
			capture = backend.CollectCrashLoop(captureCtx, candidates[0].Ref, "checkout", nil)
			lastError = validateCapture(capture)
			if lastError == nil {
				break
			}
		}
		if attempt < 4 {
			select {
			case <-captureCtx.Done():
			case <-time.After(3 * time.Second):
			}
		}
	}
	report.CaptureDurationMS = time.Since(start).Milliseconds()
	if capture != nil {
		report.Target, report.Sources = capture.Target, sourceSummaries(capture)
	}
	if lastError != nil {
		fail(lastError.Error())
	}
	if capture == nil {
		fail("no capture was produced within the lab validation budget")
	}
	report.CaptureValidated = true
	frozen := snapshotBackend{Backend: backend, capture: capture}
	settings := config.DefaultModelSettings()
	deterministic := agent.New(&frozen, settings, false)
	answer := deterministic.InvestigateCrashLoop(context.Background(), capture.Target, "checkout", nil)
	deterministic.Close()
	ref, err := validateAnswer(answer)
	if err != nil {
		fail(err.Error())
	}
	report.DeterministicCitationsValidated = true
	report.ExactFailureCitation, report.KnownFailureExcerpt = ref, expectedFailure
	t.Logf("Verified %d real captured sources and exact citations in %d ms (%d attempt(s))", len(report.Sources), report.CaptureDurationMS, report.CaptureAttempts)
	if modelURL == "" {
		t.Logf("Real capture verified in %d ms across %d attempt(s); model validation not requested", report.CaptureDurationMS, report.CaptureAttempts)
		return
	}
	settings.BaseURL, settings.Model = modelURL, modelName
	settings.Timeout, settings.InvestigationTimeout = 300*time.Second, 360*time.Second
	httpModel := agent.NewHTTPModel(settings)
	defer httpModel.Close()
	tracked := &observedModel{Model: httpModel}
	withModel := agent.NewWithModel(&frozen, settings, false, tracked)
	defer withModel.Close()
	modelCtx, modelCancel := context.WithTimeout(context.Background(), 360*time.Second)
	defer modelCancel()
	t.Log("Requesting one local-model summary; CPU inference may take several minutes")
	start = time.Now()
	progress := []string{}
	answer = withModel.InvestigateCrashLoop(modelCtx, capture.Target, "checkout", func(message string) {
		if len(progress) < 16 {
			progress = append(progress, redact.Text(message, 500))
		}
	})
	report.Model.DurationMS = time.Since(start).Milliseconds()
	report.Model.Requests, report.Model.ToolsOffered = tracked.calls, tracked.tools
	report.Model.Answer = redact.Text(answer.Text, 12000)
	report.Model.Diagnostic = tracked.diagnostic(progress)
	if tracked.calls != 1 || tracked.tools != 0 || tracked.failed || !strings.HasPrefix(answer.Text, "**Captured observations**\n") {
		report.Model.Status = "fallback"
		fail("real model summary was unavailable or rejected; capture passed, model validation did not")
	}
	report.Model.Status = "accepted"
	if _, err := validateAnswer(answer); err != nil {
		report.Model.Status = "accepted_but_scenario_missed"
		fail("model summary was accepted but did not include the exact previous-log citation required by this demo")
	}
	report.Model.KnownFailureClaimCited = modelCitesKnownFailure(tracked.response.Content, answer.Investigation)
	if !report.Model.KnownFailureClaimCited {
		report.Model.Status = "accepted_but_scenario_missed"
		fail("model summary passed structural validation but did not cite the known missing DATABASE_URL failure")
	}
	t.Logf("Real capture passed; single model summary accepted in %d ms and cited the known fixture failure", report.Model.DurationMS)
}

// Slow local inference interprets exactly the already validated sources. This
// mirrors the application's capture-then-summarize path without recollecting a
// rapidly restarting workload simply to repeat the same assertions.
type snapshotBackend struct {
	core.Backend
	capture *core.Investigation
}

func (b *snapshotBackend) CollectCrashLoop(_ context.Context, target core.ResourceRef, _ string, _ func(string)) *core.Investigation {
	if target != b.capture.Target {
		return nil
	}
	return b.capture.Clone()
}

type observedModel struct {
	agent.Model
	calls, tools int
	failed       bool
	response     agent.Message
	requestError string
	citationIDs  []string
	structured   bool
}

func (m *observedModel) Complete(ctx context.Context, messages []agent.Message, tools []agent.ToolDefinition) (agent.Message, error) {
	m.calls++
	m.tools += len(tools)
	m.citationIDs = transmittedIDs(messages)
	message, err := m.Model.Complete(ctx, messages, tools)
	return m.recordResponse(message, err)
}

func (m *observedModel) CompleteStructured(ctx context.Context, messages []agent.Message, schema json.RawMessage) (agent.Message, error) {
	m.calls++
	m.structured = true
	m.citationIDs = transmittedIDs(messages)
	structured, ok := m.Model.(agent.StructuredModel)
	if !ok {
		return m.recordResponse(agent.Message{}, &agent.ModelError{Message: "Observed model does not support structured summaries"})
	}
	message, err := structured.CompleteStructured(ctx, messages, schema)
	return m.recordResponse(message, err)
}

func (m *observedModel) recordResponse(message agent.Message, err error) (agent.Message, error) {
	m.response, m.failed = message, err != nil
	if err != nil {
		var safe *agent.ModelError
		m.requestError = "Model request failed"
		if errors.As(err, &safe) {
			m.requestError = redact.Text(safe.Message, 500)
		}
	}
	return message, err
}

func (m *observedModel) diagnostic(progress []string) *modelDiagnostic {
	const responseCharacters = 8000
	display := redact.Text(m.response.Content, -1)
	return &modelDiagnostic{
		StructuredRequest: m.structured,
		ResponseContent:   redact.Sanitize(display, responseCharacters),
		ResponseBytes:     len(m.response.Content), ResponseDisplayBounded: len([]rune(display)) > responseCharacters,
		ResponseRole: redact.Text(m.response.Role, 50), ReturnedToolCalls: len(m.response.ToolCalls),
		RequestError: m.requestError, TransmittedCitationIDs: append([]string(nil), m.citationIDs...),
		Progress: append([]string(nil), progress...),
	}
}

func transmittedIDs(messages []agent.Message) []string {
	ids := []string{}
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		_, payload, found := strings.Cut(message.Content, "\n\nCaptured evidence:\n")
		if !found {
			continue
		}
		var capture struct {
			Evidence []struct {
				ID string `json:"id"`
			} `json:"evidence"`
		}
		if json.Unmarshal([]byte(payload), &capture) != nil {
			continue
		}
		for _, entry := range capture.Evidence {
			// Generated IDs only. Never preserve arbitrary model prompt fields.
			if len(entry.ID) <= 5 && referencePattern.FindString("["+entry.ID+"]") == "["+entry.ID+"]" && len(ids) < 200 {
				ids = append(ids, entry.ID)
			}
		}
	}
	return ids
}

func repoRoot() (string, error) {
	path, err := os.Getwd()
	if err != nil {
		return "", errors.New("cannot determine repository root")
	}
	for range 6 {
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
			return path, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return "", errors.New("run lab validation from this repository")
}

func writeReport(path string, report validationReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return errors.New("could not serialize report")
	}
	if len(data) > 64*1024 {
		return errors.New("report exceeded 64 KiB")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("could not create report directory")
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return errors.New("could not save report")
	}
	return nil
}
