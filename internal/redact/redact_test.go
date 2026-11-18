package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactsCredentialsPreservingDiagnostics(t *testing.T) {
	original := `database connection failed password="super secret" token=abc123 Authorization: Bearer aaa.bbb.ccc uri=postgresql://alice:pass@db:5432/app api_key: "my-api-key" error=ECONNREFUSED`
	result := Text(original, 16000)
	for _, secret := range []string{"super secret", "abc123", "aaa.bbb.ccc", "alice:pass", "my-api-key"} {
		if strings.Contains(result, secret) {
			t.Errorf("secret leaked: %s", secret)
		}
	}
	if !strings.Contains(result, "ECONNREFUSED") || !strings.Contains(result, "db:5432/app") {
		t.Fatal("diagnostics lost")
	}
}

func TestRecursiveRedaction(t *testing.T) {
	result := Value(map[string]any{"client_secret": "s3cret", "rows": []any{map[string]any{"authorization": "anything", "message": "password=hidden OOMKilled"}}, "status": "CrashLoopBackOff"}, 16000)
	data, _ := json.Marshal(result)
	for _, secret := range []string{"s3cret", "anything", "hidden"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("secret leaked: %s", secret)
		}
	}
	if !strings.Contains(string(data), "OOMKilled") {
		t.Fatal("diagnosis lost")
	}
}

func TestPEMJWTAndTerminalControls(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\nabcdef\n-----END RSA PRIVATE KEY-----"
	if strings.Contains(Text(key, 20), "abcdef") {
		t.Fatal("PEM leaked")
	}
	if strings.Contains(Text("id=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJldmFuIn0.signature", 16000), "eyJ") {
		t.Fatal("JWT leaked")
	}
	if got := Text("\x1b[31mFailure\x1b[0m\x00", 100); got != "Failure" {
		t.Fatalf("escape stripping: %q", got)
	}
	if got := Text("\x1b]52;c;ZXhmaWw=\x07diagnostic", 100); got != "diagnostic" {
		t.Fatalf("OSC stripping: %q", got)
	}
}

func TestBoundsAndUnicode(t *testing.T) {
	if len([]rune(Text(strings.Repeat("界", 100), 10))) > 30 {
		t.Fatal("text not bounded")
	}
	items := make([]any, 150)
	if len(Value(items, 100).([]any)) != 100 {
		t.Fatal("list not bounded")
	}
}
