// 频道参与者（房主视角频道管理用）。
package api

import (
	"net/http"

	"hearth/server/internal/rtc"
)

// ---- 频道参与者（房主视角频道管理用）----

func (a *API) channelParticipants(w http.ResponseWriter, r *http.Request) {
	c := channelFrom(r)
	// 两条线的参与者并集（OBS 推流只在舞台线；同一设备两线 identity 一致会去重）
	seen := map[string]bool{}
	ps := []rtc.Participant{}
	_, vp := a.voiceInstance(r.Context())
	providers := []rtc.Provider{vp}
	if _, sp := a.stageInstance(r.Context()); sp != nil {
		providers = append(providers, sp)
	}
	for _, pr := range providers {
		if list, err := pr.ListParticipants(r.Context(), c.Name); err == nil {
			for _, x := range list {
				if !seen[x.Identity] {
					seen[x.Identity] = true
					ps = append(ps, x)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"participants": ps})
}
