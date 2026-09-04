package perm

import (
	"testing"

	"hearth/server/internal/store"
)

func TestCanActOnChannelRequiresStrictlyHigherRole(t *testing.T) {
	tests := []struct {
		name          string
		actor, target store.ChannelRole
		want          bool
	}{
		{"owner over moderator", store.ChannelRoleOwner, store.ChannelRoleModerator, true},
		{"moderator over member", store.ChannelRoleModerator, store.ChannelRoleMember, true},
		{"moderator over none", store.ChannelRoleModerator, store.ChannelRoleNone, true},
		{"moderator beside moderator", store.ChannelRoleModerator, store.ChannelRoleModerator, false},
		{"moderator below owner", store.ChannelRoleModerator, store.ChannelRoleOwner, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanActOnChannel(tt.actor, tt.target); got != tt.want {
				t.Fatalf("CanActOnChannel(%q, %q)=%v, want %v", tt.actor, tt.target, got, tt.want)
			}
		})
	}
}
