package kube

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"kubix/internal/core"
)

const maxServerCursorBytes = 4096

type resourceCursor struct {
	Session   string `json:"s"`
	Version   int    `json:"v"`
	Kind      string `json:"k"`
	Namespace string `json:"n"`
	Query     string `json:"q"`
	Limit     int    `json:"l"`
	Continue  string `json:"c"`
}

func (b *Backend) encodeResourceCursor(kind, namespace, query string, limit int, continuation string) (string, error) {
	if continuation == "" {
		return "", nil
	}
	if len(continuation) > maxServerCursorBytes {
		return "", errors.New("The server continuation exceeded the cursor budget; narrow or refresh the resource view")
	}
	session, err := b.resourceCursorSession()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(resourceCursor{Version: 1, Session: session, Kind: kind, Namespace: namespace, Query: query, Limit: limit, Continue: continuation})
	if err != nil {
		return "", errors.New("Unable to encode continuation; refresh the resource view")
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > core.MaxResourceCursorBytes {
		return "", errors.New("The continuation exceeded the cursor budget; narrow or refresh the resource view")
	}
	return encoded, nil
}

// ListResourcesPage is an explicitly requested single page. The envelope binds
// an opaque server continuation to its original view; it is not an authorization
// mechanism. Namespace RBAC and the Backend's fixed scope still enforce access.
func (b *Backend) ListResourcesPage(ctx context.Context, kind, namespace, query string, limit int, cursor string) core.ResourcePage {
	reject := func(message string) core.ResourcePage {
		return core.ResourcePage{Result: invalid("list_resources", message)}
	}
	scope, err := b.scope(kind, namespace)
	if err != nil {
		return reject(err.Error())
	}
	if limit < 1 || limit > maxList {
		return reject("limit must be between 1 and 100")
	}
	if len([]rune(query)) > 200 {
		return reject("query must be at most 200 characters")
	}
	if ctx.Err() != nil {
		return core.ResourcePage{Result: failure("list_resources", ctx.Err())}
	}
	rawCursor := ""
	if cursor != "" {
		if len(cursor) > core.MaxResourceCursorBytes {
			return reject("Continuation exceeds the cursor budget; refresh the resource view")
		}
		data, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return reject("Invalid continuation; refresh the resource view")
		}
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		var value resourceCursor
		if d.Decode(&value) != nil {
			return reject("Invalid continuation; refresh the resource view")
		}
		var trailing any
		if d.Decode(&trailing) != io.EOF || value.Version != 1 || value.Continue == "" || len(value.Continue) > maxServerCursorBytes {
			return reject("Invalid continuation; refresh the resource view")
		}
		if value.Kind != kind || value.Namespace != scope || value.Query != query || value.Limit != limit {
			return reject("Continuation belongs to a different resource view; refresh before changing kind, namespace, query or page size")
		}
		session, sessionErr := b.resourceCursorSession()
		if sessionErr != nil {
			return reject(sessionErr.Error())
		}
		if value.Session != session {
			return reject("Continuation belongs to a different backend session; refresh the resource view")
		}
		rawCursor = value.Continue
	}
	return b.listResourcesPage(ctx, kind, scope, query, limit, rawCursor, true)
}

var _ core.ResourcePageReader = (*Backend)(nil)

func (b *Backend) resourceCursorSession() (string, error) {
	b.paginationOnce.Do(func() {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			b.paginationSessionErr = errors.New("Unable to initialize pagination session; refresh the resource view")
			return
		}
		b.paginationSession = hex.EncodeToString(token[:])
	})
	return b.paginationSession, b.paginationSessionErr
}

func resourcePageFailure(err error) core.ToolResult {
	if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
		return invalid("list_resources", "The resource continuation expired (410); refresh to start a new snapshot")
	}
	return failure("list_resources", err)
}
