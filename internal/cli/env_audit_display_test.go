package cli

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAuditListOutputIsValidUTF8(t *testing.T) {
	f, dir, _ := setupEnvReady(t)
	code, out, errb := envListRun(t, f, dir)
	if code != ExitOK {
		t.Fatalf("rc=%d err=%s", code, errb)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "CURRENT") {
			t.Logf("row=%q utf8=%v", line, utf8.ValidString(line))
		}
	}
	if !utf8.ValidString(out) {
		t.Fatal("default gen0 label is sliced inside a UTF-8 character")
	}
}
