package api

import (
	"context"
	"errors"
	"regexp"
	"strconv"

	"hearth/server/internal/store"
)

// numericRe 纯数字：既是频道引用「先按 id」的判据，也是新建频道的禁用名规则。
var numericRe = regexp.MustCompile(`^[0-9]+$`)

// channelByRef 解析频道引用（HTTP 路径段 / 请求体）：纯数字先按 id，找不到再按名字；
// 其余一律按名字。新地址一律用 id，OBS 里已存的名字地址继续有效；
// 既有纯数字名的频道靠名字兜底工作（新建频道已禁止纯数字名，不会再新增歧义）。
func (a *API) channelByRef(ctx context.Context, ref string) (*store.Channel, error) {
	if numericRe.MatchString(ref) {
		if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
			c, err := a.st.ChannelByID(ctx, id)
			if err == nil {
				return c, nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		}
	}
	return a.st.ChannelByName(ctx, ref)
}
