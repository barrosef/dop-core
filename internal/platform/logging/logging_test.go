package logging

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// O core e o BFF precisam escrever os MESMOS campos canônicos. Log agregado só
// é útil se as duas pontas falarem a mesma língua.
func TestFormatoJSONCanonico(t *testing.T) {
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w

	log := New("serve")
	log.Info("rpc concluída",
		"rpc", "/dop.v1.IdentityService/GetUser",
		FieldRequestID, "trace-demo-1",
		FieldAccountID, "acct-1",
		FieldDurationMs, 12,
		"token", "deveria-sumir",
	)

	w.Close()
	os.Stdout = stdout
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	line := strings.TrimSpace(string(buf[:n]))

	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("saída não é JSON válido: %v\n%s", err, line)
	}

	for _, campo := range []string{"ts", "level", "msg", FieldComponent, FieldMode, FieldRequestID, FieldAccountID, FieldDurationMs} {
		if _, ok := got[campo]; !ok {
			t.Errorf("campo canônico ausente: %s", campo)
		}
	}
	if got[FieldComponent] != "dop-core" {
		t.Errorf("component deveria ser dop-core, veio %v", got[FieldComponent])
	}
	if got["token"] != "***" {
		t.Errorf("SEGREDO VAZOU no log: token = %v", got["token"])
	}
}
