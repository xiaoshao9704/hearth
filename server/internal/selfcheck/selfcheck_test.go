package selfcheck

import "testing"

func TestURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8090":            "http://127.0.0.1:8090/healthz",
		"0.0.0.0:58080":    "http://127.0.0.1:58080/healthz",
		"[::]:8080":        "http://127.0.0.1:8080/healthz",
		"192.168.1.5:9000": "http://192.168.1.5:9000/healthz",
	} {
		if got := URL(addr); got != want {
			t.Errorf("URL(%q) = %q, 期望 %q", addr, got, want)
		}
	}
}
