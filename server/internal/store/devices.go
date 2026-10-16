package store

import (
	"context"
)

// ---- 设备 ----

// RecordDevice 记录用户设备：首次出现建档，之后刷新 last_seen 与设备标签。
// upsert 的方言分叉是全 store 仅保留的两处之一（另一处是 admin.go 的 SetSetting）：mysql 无 ON CONFLICT，用 DUPLICATE KEY UPDATE + VALUES()。
func (s *Store) RecordDevice(ctx context.Context, userID int64, deviceID, tag string) error {
	row := &deviceRow{UserID: userID, DeviceID: deviceID, Tag: tag}
	q := s.bun.NewInsert().Model(row)
	if s.d.name == "mysql" {
		q = q.On("DUPLICATE KEY UPDATE").Set("last_seen = CURRENT_TIMESTAMP, tag = VALUES(tag)")
	} else { // sqlite / postgres 同语法
		q = q.On("CONFLICT (user_id, device_id) DO UPDATE").Set("last_seen = CURRENT_TIMESTAMP, tag = EXCLUDED.tag")
	}
	_, err := q.Exec(ctx)
	return err
}
