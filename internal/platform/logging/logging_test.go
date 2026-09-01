package logging

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The core and the BFF must write the SAME canonical fields. An aggregated log
// is only useful if both ends speak the same language.
func TestCanonicalJSONFormat(t *testing.T) {
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w

	log := New("serve")
	log.Info("rpc completed",
		"rpc", "/dop.v1.IdentityService/GetUser",
		FieldRequestID, "trace-demo-1",
		FieldAccountID, "acct-1",
		FieldDurationMs, 12,
		"token", "should-disappear",
	)

	w.Close()
	os.Stdout = stdout
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	line := strings.TrimSpace(string(buf[:n]))

	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, line)
	}

	for _, field := range []string{"ts", "level", "msg", FieldComponent, FieldMode, FieldRequestID, FieldAccountID, FieldDurationMs} {
		if _, ok := got[field]; !ok {
			t.Errorf("canonical field missing: %s", field)
		}
	}
	if got[FieldComponent] != "dop-core" {
		t.Errorf("component should be dop-core, got %v", got[FieldComponent])
	}
	if got["token"] != "***" {
		t.Errorf("SECRET LEAKED into the log: token = %v", got["token"])
	}
}
