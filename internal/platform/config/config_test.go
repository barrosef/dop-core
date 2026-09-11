package config

import "testing"

// The accepted MAIL_BACKEND values and the ones the composition root actually
// wires are TWO LISTS, in two files, and nothing tied them together.
//
// They drifted the first time a provider was added: wire.go gained a `case
// "onesignal"` and this validation kept refusing the word, so the process
// exited at boot with "unknown MAIL_BACKEND" — a deploy that fails loudly, at
// least, but only after an image had been built, pushed and rolled out.
//
// This test is the tie. Adding a backend means adding it here, which is the
// same file the reader of the switch is already looking at.
func TestEveryWiredMailBackendIsAccepted(t *testing.T) {
	// The list wire.go switches on. Keep them together.
	for _, backend := range []string{"onesignal", "sendgrid", "smtp"} {
		t.Setenv("DATABASE_URL", "postgres://dop@localhost:5432/dop")
		t.Setenv("MAIL_BACKEND", backend)

		if _, err := Load("serve"); err != nil {
			t.Errorf("MAIL_BACKEND=%q is wired in wire.go and refused here: %v", backend, err)
		}
	}
}

func TestAnUnknownMailBackendIsRefusedAtBoot(t *testing.T) {
	// Failing closed is the point: a typo that fell through to a default would
	// be discovered on the first message that never arrives, with the process
	// green for weeks.
	t.Setenv("DATABASE_URL", "postgres://dop@localhost:5432/dop")
	t.Setenv("MAIL_BACKEND", "sengrid")

	if _, err := Load("serve"); err == nil {
		t.Fatal("a misspelled backend was accepted")
	}
}
