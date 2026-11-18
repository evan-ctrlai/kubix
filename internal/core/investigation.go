package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Observation holds an immutable serialized read. Later Kubernetes reads never
// replace Payload. Resource payloads are sanitized Resource JSON; logs are text.
type Observation struct {
	ID              string      `json:"id"`
	Title           string      `json:"title"`
	Source          string      `json:"source"`
	Ref             ResourceRef `json:"ref"`
	ResourceVersion string      `json:"resource_version,omitempty"`
	CapturedAt      time.Time   `json:"captured_at"`
	State           string      `json:"state"` // captured, sampled, unavailable, changed
	Query           string      `json:"query"`
	Format          string      `json:"format"` // json or log
	Payload         string      `json:"payload"`
	Errors          []string    `json:"errors,omitempty"`
	Notes           []string    `json:"notes,omitempty"`
}

// EvidenceCitation addresses a captured observation, an RFC6901 JSON pointer,
// or an inclusive range of one-based log lines. IDs are local to an investigation.
type EvidenceCitation struct {
	ID            string `json:"id"`
	ObservationID string `json:"observation_id"`
	Label         string `json:"label"`
	JSONPointer   string `json:"json_pointer,omitempty"`
	LineStart     int    `json:"line_start,omitempty"`
	LineEnd       int    `json:"line_end,omitempty"`
}

type Investigation struct {
	ID           string             `json:"id"`
	Kind         string             `json:"kind"`
	Context      string             `json:"context"`
	Namespace    string             `json:"namespace"`
	Target       ResourceRef        `json:"target"`
	StartedAt    time.Time          `json:"started_at"`
	FinishedAt   time.Time          `json:"finished_at"`
	Resources    []Resource         `json:"resources"`
	Observations []Observation      `json:"observations"`
	Citations    []EvidenceCitation `json:"citations"`
	Limitations  []string           `json:"limitations,omitempty"`
}

// CrashLoopCollector is an optional, narrowly scoped read-only capability.
// Implementations must sanitize and bound payloads before capturing them.
type CrashLoopCollector interface {
	CollectCrashLoop(ctx context.Context, target ResourceRef, container string, onEvent func(string)) *Investigation
}

// CrashLoopInvestigator lets the terminal pass an explicit container choice
// without encoding tool arguments in the user's natural-language question.
type CrashLoopInvestigator interface {
	InvestigateCrashLoop(ctx context.Context, target ResourceRef, container string, onEvent func(string)) Answer
}

var investigationSequence atomic.Uint64

func NewInvestigation(contextName, namespace string, target ResourceRef) *Investigation {
	now := time.Now().UTC()
	return &Investigation{
		ID:   fmt.Sprintf("crashloop-%d-%d", now.UnixNano(), investigationSequence.Add(1)),
		Kind: "crashloop", Context: contextName, Namespace: namespace, Target: target, StartedAt: now,
	}
}

// AddObservation captures a source and creates its full-observation citation.
func (i *Investigation) AddObservation(observation Observation) EvidenceCitation {
	observation.ID = fmt.Sprintf("O%d", len(i.Observations)+1)
	if observation.CapturedAt.IsZero() {
		observation.CapturedAt = time.Now().UTC()
	}
	observation.Errors = append([]string(nil), observation.Errors...)
	observation.Notes = append([]string(nil), observation.Notes...)
	i.Observations = append(i.Observations, observation)
	citation := EvidenceCitation{ID: fmt.Sprintf("E%d", len(i.Citations)+1), ObservationID: observation.ID, Label: observation.Title}
	i.Citations = append(i.Citations, citation)
	return citation
}

// AddCitation validates a narrow target before making it available to the model.
func (i *Investigation) AddCitation(citation EvidenceCitation) (EvidenceCitation, error) {
	if len(i.Citations) >= 200 {
		return EvidenceCitation{}, errors.New("citation limit reached")
	}
	if citation.LineStart > 0 && citation.LineEnd == 0 {
		citation.LineEnd = citation.LineStart
	}
	var observation *Observation
	for n := range i.Observations {
		if i.Observations[n].ID == citation.ObservationID {
			observation = &i.Observations[n]
			break
		}
	}
	if observation == nil || !validCitationTarget(*observation, citation) {
		return EvidenceCitation{}, errors.New("citation target does not exist in captured evidence")
	}
	citation.ID = fmt.Sprintf("E%d", len(i.Citations)+1)
	i.Citations = append(i.Citations, citation)
	return citation, nil
}

func (i *Investigation) ResolveCitation(id string) (Observation, EvidenceCitation, bool) {
	if i == nil {
		return Observation{}, EvidenceCitation{}, false
	}
	for _, citation := range i.Citations {
		if citation.ID != id {
			continue
		}
		for _, observation := range i.Observations {
			if observation.ID == citation.ObservationID && validCitationTarget(observation, citation) {
				return observation, citation, true
			}
		}
	}
	return Observation{}, EvidenceCitation{}, false
}

func validCitationTarget(observation Observation, citation EvidenceCitation) bool {
	if citation.JSONPointer != "" {
		if observation.Format != "json" || citation.LineStart != 0 || citation.LineEnd != 0 {
			return false
		}
		_, ok := JSONPointerValue(observation.Payload, citation.JSONPointer)
		return ok
	}
	if citation.LineStart != 0 || citation.LineEnd != 0 {
		if observation.Format != "log" || citation.LineStart < 1 || citation.LineEnd < citation.LineStart {
			return false
		}
		lines := strings.Split(strings.TrimSuffix(observation.Payload, "\n"), "\n")
		return observation.Payload != "" && citation.LineEnd <= len(lines)
	}
	return true
}

// JSONPointerValue resolves an exact captured JSON field without evaluating an
// expression or consulting a live resource.
func JSONPointerValue(payload, pointer string) (any, bool) {
	var value any
	if json.Unmarshal([]byte(payload), &value) != nil {
		return nil, false
	}
	if pointer == "" {
		return value, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	for _, part := range strings.Split(pointer[1:], "/") {
		for n := 0; n < len(part); n++ {
			if part[n] == '~' {
				if n+1 >= len(part) || (part[n+1] != '0' && part[n+1] != '1') {
					return nil, false
				}
				n++
			}
		}
		key := strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch node := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = node[key]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(node) || strconv.Itoa(index) != key {
				return nil, false
			}
			value = node[index]
		default:
			return nil, false
		}
	}
	return value, true
}

// Clone gives a conversation turn its own evidence metadata and browser rows.
// Immutable serialized payload strings can be shared safely between copies.
func (i *Investigation) Clone() *Investigation {
	if i == nil {
		return nil
	}
	clone := *i
	clone.Observations = append([]Observation(nil), i.Observations...)
	for n := range clone.Observations {
		clone.Observations[n].Errors = append([]string(nil), i.Observations[n].Errors...)
		clone.Observations[n].Notes = append([]string(nil), i.Observations[n].Notes...)
	}
	clone.Citations = append([]EvidenceCitation(nil), i.Citations...)
	clone.Limitations = append([]string(nil), i.Limitations...)
	clone.Resources = append([]Resource(nil), i.Resources...)
	for n := range clone.Resources {
		data, err := json.Marshal(i.Resources[n].Details)
		clone.Resources[n].Details = nil
		if err == nil {
			_ = json.Unmarshal(data, &clone.Resources[n].Details)
		}
	}
	return &clone
}
