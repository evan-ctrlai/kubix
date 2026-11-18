// Package labtest also provides independent, opt-in real-cluster scenario checks.
package labtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"k8s.io/client-go/tools/clientcmd"
	"kubix/internal/agent"
	"kubix/internal/config"
	"kubix/internal/core"
	"kubix/internal/kube"
	"kubix/internal/redact"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ScenarioOptions struct {
	Root, Scenario, Prefix, ModelURL, Model string
	AllowLive                               bool
}
type ScenarioReport struct {
	SchemaVersion     int             `json:"schema_version"`
	Scenario          string          `json:"scenario"`
	StartedAt         time.Time       `json:"started_at"`
	FinishedAt        time.Time       `json:"finished_at"`
	InvestigationMS   int64           `json:"investigation_duration_ms"`
	ModelConfigured   bool            `json:"model_configured"`
	ModelRequests     int             `json:"model_requests"`
	Mode              string          `json:"mode"`
	Passed            bool            `json:"passed"`
	Checks            map[string]bool `json:"checks"`
	CapturedSources   int             `json:"captured_sources"`
	AvailableSources  int             `json:"available_sources"`
	RenderedCitations int             `json:"rendered_citations"`
	Answer            string          `json:"answer"`
	Failure           string          `json:"failure,omitempty"`
	Limitations       []string        `json:"limitations"`
}

var scenarioPrefix = regexp.MustCompile(`^kubix-check-[a-f0-9]{12}$`)
var allReferences = regexp.MustCompile(`\[(E[0-9]+|R[0-9]+)\]`)

func validScenarioOptions(o ScenarioOptions) error {
	if !o.AllowLive {
		return errors.New("live scenarios require explicit opt-in through labs/scenarios/run.ps1")
	}
	if o.Scenario != "stale-current" && o.Scenario != "healthy-history" && o.Scenario != "service-mismatch" {
		return errors.New("unknown scenario")
	}
	if !scenarioPrefix.MatchString(o.Prefix) {
		return errors.New("fixture prefix must use the owned scenario format")
	}
	if (o.ModelURL == "") != (o.Model == "") {
		return errors.New("supply both local model URL and model name, or neither")
	}
	if o.ModelURL != "" && (!loopbackURL(o.ModelURL, "", "/v1") || len(o.Model) > 100 || strings.ContainsAny(o.Model, "\r\n\x1b") || strings.Contains(strings.ToLower(o.Model), "cloud")) {
		return errors.New("only an explicitly configured loopback local model is permitted")
	}
	return nil
}

type scenarioModel struct {
	inner *agent.HTTPModel
	calls int
}

func (m *scenarioModel) Complete(c context.Context, msg []agent.Message, tools []agent.ToolDefinition) (agent.Message, error) {
	m.calls++
	return m.inner.Complete(c, msg, tools)
}
func (m *scenarioModel) CompleteStructured(c context.Context, msg []agent.Message, schema json.RawMessage) (agent.Message, error) {
	m.calls++
	return m.inner.CompleteStructured(c, msg, schema)
}
func (m *scenarioModel) Close() { m.inner.Close() }

// RunScenario only reads the dedicated lab. Fixture lifecycle/ownership is kept
// in the operator script; the application never receives administrator access.
func RunScenario(o ScenarioOptions) (report ScenarioReport, err error) {
	report = ScenarioReport{SchemaVersion: 1, Scenario: o.Scenario, StartedAt: time.Now().UTC(), ModelConfigured: o.ModelURL != "", Mode: "deterministic_evidence", Checks: map[string]bool{}, Limitations: []string{"One controlled synthetic incident on a real isolated cluster; not a production incident or accuracy benchmark.", "Elapsed investigation time excludes fixture setup, Go build and model startup. Captured sources are not an API-operation count.", "Passed checks validate this fixture and source provenance, not arbitrary model semantics or terminal UI behavior.", "Service scenario requires existing EndpointSlice denial; permitted endpoint access is not silently changed to create a denial."}}
	defer func() {
		report.FinishedAt = time.Now().UTC()
		if err != nil {
			report.Failure = redact.Text(err.Error(), 1000)
		}
	}()
	if err = validScenarioOptions(o); err != nil {
		return
	}
	root, e := filepath.Abs(o.Root)
	if e != nil {
		err = errors.New("invalid repository root")
		return
	}
	cfgPath := filepath.Join(root, ".local", "lab", "reader.kubeconfig")
	cfg, e := clientcmd.LoadFromFile(cfgPath)
	if e != nil {
		err = errors.New("cannot load isolated lab reader configuration")
		return
	}
	if err = validateReaderConfig(cfg); err != nil {
		return
	}
	b, e := kube.New(labContext, labNamespace, cfgPath)
	if e != nil {
		err = errors.New("cannot initialize lab reader")
		return
	}
	settings := config.DefaultModelSettings()
	settings.BaseURL = o.ModelURL
	settings.Model = o.Model
	settings.Timeout = 120 * time.Second
	settings.InvestigationTimeout = 180 * time.Second
	var model *scenarioModel
	var m agent.Model
	if o.ModelURL != "" {
		model = &scenarioModel{inner: agent.NewHTTPModel(settings)}
		m = model
		defer model.Close()
	}
	investigator := agent.NewWithModel(b, settings, false, m)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	name := o.Prefix + "-pod"
	kind := "pods"
	if o.Scenario == "service-mismatch" {
		name = o.Prefix + "-svc"
		kind = "services"
	}
	start := time.Now()
	target := b.GetResource(ctx, kind, labNamespace, name)
	if len(target.Errors) != 0 || len(target.Resources) != 1 || target.Resources[0].Ref.Name != name || target.Resources[0].Ref.Namespace != labNamespace || target.Resources[0].Ref.UID == "" {
		err = errors.New("fixture identity could not be read using the lab reader")
		return
	}
	var answer core.Answer
	if o.Scenario == "service-mismatch" {
		ref := target.Resources[0].Ref
		answer = investigator.Ask(ctx, fmt.Sprintf("Why does service %s not route to Ready pod %s? Compare selectors and labels and preserve EndpointSlice permission gaps.", name, o.Prefix+"-pod"), &ref, nil)
	} else {
		answer = investigator.InvestigateCrashLoop(ctx, target.Resources[0].Ref, "app", nil)
	}
	report.InvestigationMS = time.Since(start).Milliseconds()
	if model != nil {
		report.ModelRequests = model.calls
	}
	if report.ModelRequests > 0 {
		report.Mode = "model_invoked"
	}
	report.Answer = redact.Text(answer.Text, 16000)
	report.Checks = scenarioChecks(o.Scenario, o.Prefix, answer, report.ModelRequests)
	if answer.Investigation != nil {
		report.CapturedSources = len(answer.Investigation.Observations)
		for _, src := range answer.Investigation.Observations {
			if src.State == "captured" || src.State == "sampled" {
				report.AvailableSources++
			}
		}
	} else {
		report.CapturedSources = len(answer.Evidence)
		for _, src := range answer.Evidence {
			if len(src.Errors) == 0 {
				report.AvailableSources++
			}
		}
	}
	report.RenderedCitations = len(allReferences.FindAllString(answer.Text, -1))
	report.Passed = true
	for _, ok := range report.Checks {
		report.Passed = report.Passed && ok
	}
	if !report.Passed {
		err = errors.New("one or more scenario checks failed; inspect the saved report")
	}
	return
}
func scenarioChecks(scenario, prefix string, a core.Answer, modelCalls int) map[string]bool {
	checks := map[string]bool{"no_model_diagnosis": modelCalls == 0, "answer_present": strings.TrimSpace(a.Text) != ""}
	if scenario == "service-mismatch" {
		mismatch := strings.Contains(a.Text, prefix+"-pod") && strings.Contains(a.Text, "validation-wrong") && strings.Contains(a.Text, "validation-real") && strings.Contains(strings.ToLower(a.Text), "does not match")
		denied := false
		for _, e := range a.Evidence {
			for _, gap := range e.Errors {
				denied = denied || strings.Contains(gap, "403")
			}
		}
		checks["target_selector_mismatch_reported"] = mismatch
		checks["denied_read_retained"] = denied && strings.Contains(a.Text, "403")
		checks["denial_not_absence"] = strings.Contains(strings.ToLower(a.Text), "does not establish") && strings.Contains(strings.ToLower(a.Text), "absent")
		refs := regexp.MustCompile(`\[R([0-9]+)\]`).FindAllStringSubmatch(a.Text, -1)
		valid := len(refs) > 0
		cited := map[string]bool{}
		for _, ref := range refs {
			n, e := strconv.Atoi(ref[1])
			if e != nil || n < 1 || n > len(a.Resources) {
				valid = false
				continue
			}
			r := a.Resources[n-1]
			if r.Ref.UID == "" || r.Ref.Namespace != labNamespace {
				valid = false
			}
			cited[r.Ref.Kind+"/"+r.Ref.Name] = true
		}
		exactPod, exactService := false, false
		for _, r := range a.Resources {
			data, _ := json.Marshal(r.Details)
			var d map[string]any
			_ = json.Unmarshal(data, &d)
			if r.Ref.Kind == "pods" && r.Ref.Name == prefix+"-pod" && r.Ref.Namespace == labNamespace {
				if labels, ok := d["labels"].(map[string]any); ok {
					exactPod = labels["app"] == "validation-real"
				}
			}
			if r.Ref.Kind == "services" && r.Ref.Name == prefix+"-svc" && r.Ref.Namespace == labNamespace {
				if selector, ok := d["selector"].(map[string]any); ok {
					exactService = selector["app"] == "validation-wrong"
				}
			}
		}
		checks["resource_citations_resolve"] = valid && cited["pods/"+prefix+"-pod"] && cited["services/"+prefix+"-svc"]
		checks["fixture_selector_evidence"] = exactPod && exactService
		return checks
	}
	inv := a.Investigation
	checks["capture_present"] = inv != nil
	if inv == nil {
		return checks
	}
	current := ""
	previousGap := false
	exit42 := false
	healthy := false
	for _, o := range inv.Observations {
		if o.Source == "kubernetes/pods/log" && strings.Contains(strings.ToLower(o.Title), "current") && (o.State == "sampled" || o.State == "captured") {
			current += o.Payload
		}
		if o.Source == "kubernetes/pods/log" && strings.Contains(strings.ToLower(o.Title), "previous") && o.State == "unavailable" {
			previousGap = true
		}
		if o.Source == "kubernetes/pods" && o.State == "captured" {
			v, ok := core.JSONPointerValue(o.Payload, "/details/containers/0/state/terminated/exit_code")
			exit42 = exit42 || (ok && fmt.Sprint(v) == "42")
			status, _ := core.JSONPointerValue(o.Payload, "/status")
			ready, _ := core.JSONPointerValue(o.Payload, "/details/containers/0/ready")
			count, _ := core.JSONPointerValue(o.Payload, "/details/containers/0/restart_count")
			healthy = healthy || (status == "Running" && ready == true && fmt.Sprint(count) == "0")
		}
	}
	checks["missing_prior_logs_captured"] = previousGap
	checks["missing_prior_logs_visible"] = strings.Contains(strings.ToLower(a.Text), "previous") && strings.Contains(strings.ToLower(a.Text), "unavailable")
	valid := true
	refs := regexp.MustCompile(`\[(E[0-9]+)\]`).FindAllStringSubmatch(a.Text, -1)
	for _, r := range refs {
		_, _, ok := inv.ResolveCitation(r[1])
		valid = valid && ok
	}
	checks["rendered_citations_resolve"] = valid && len(refs) > 0
	checks["no_hypotheses_section"] = !strings.Contains(a.Text, "**Possible explanations**")
	if scenario == "stale-current" {
		checks["both_temporal_lines_captured"] = strings.Contains(current, "STALE (cached)") && strings.Contains(current, "FATAL (current)")
		checks["current_exit_42"] = exit42 && strings.Contains(a.Text, "exit 42")
		checks["current_source_label"] = strings.Contains(strings.ToLower(a.Text), "current logs")
	} else {
		checks["ready_zero_restarts"] = healthy
		checks["current_log_captured"] = strings.Contains(current, "healthy replacement running")
		checks["history_not_invented"] = strings.Contains(strings.ToLower(a.Text), "cannot be established")
	}
	return checks
}
func SaveScenarioReport(root, prefix string, r ScenarioReport) (string, error) {
	if !scenarioPrefix.MatchString(prefix) {
		return "", errors.New("invalid report prefix")
	}
	name := "validation-" + r.Scenario + "-" + prefix + ".json"
	path, err := reportPath(root, filepath.Join(root, ".local", "lab", name))
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil || len(data) > 64*1024 {
		return "", errors.New("scenario report exceeds safe bounds")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", errors.New("cannot create report directory")
	}
	if err = os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return "", errors.New("cannot save scenario report")
	}
	return path, nil
}
