package main

import (
	"os"
	"slices"
	"testing"
)

func TestPositionals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "single dash data",
			args: []string{"hearth", "-data", "<占位>", "adduser", "user", "change-me"},
			want: []string{"adduser", "user", "change-me"},
		},
		{
			name: "password starts with dash",
			args: []string{"hearth", "adduser", "bob", "-abc123", "--data", "<占位>"},
			want: []string{"adduser", "bob", "-abc123"},
		},
		{
			name: "flag terminator",
			args: []string{"hearth", "adduser", "bob", "--", "--system"},
			want: []string{"adduser", "bob", "--system"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := os.Args
			os.Args = tt.args
			t.Cleanup(func() { os.Args = old })
			if got := positionals(); !slices.Equal(got, tt.want) {
				t.Fatalf("位置参数解析错误: %v", got)
			}
		})
	}
}
