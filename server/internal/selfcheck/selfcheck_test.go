package selfcheck

import "testing"

func TestURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8090":            "http://127.0.0.1:8090/healthz?refresh=1",
		"0.0.0.0:58080":    "http://127.0.0.1:58080/healthz?refresh=1",
		"[::]:8080":        "http://127.0.0.1:8080/healthz?refresh=1",
		"192.168.1.5:9000": "http://192.168.1.5:9000/healthz?refresh=1",
	} {
		if got := URL(addr); got != want {
			t.Errorf("URL(%q) = %q, 期望 %q", addr, got, want)
		}
	}
}
