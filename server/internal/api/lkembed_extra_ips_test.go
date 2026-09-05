package api

import (
	"context"
	"slices"
	"testing"
)

func TestStageExternalIPsIncludesValidatedExtras(t *testing.T) {
	a := testAPI(t)
	if err := a.st.SetSetting(context.Background(), "cfg_lkembed_extra_ips",
		"203.0.113.7, 2001:db8::7; bad-ip 203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.7", "2001:db8::7"}
	if got := a.stageExternalIPs(); !slices.Equal(got, want) {
		t.Fatalf("额外候选=%v, want %v", got, want)
	}
}
