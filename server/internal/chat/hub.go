// 聊天 WebSocket：每频道一个房间，进房推最近 50 条历史，新消息落库后广播。
package chat

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"hearth/server/internal/store"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const historyLimit = 50

// 服务端下发的消息格式
type serverMsg struct {
	Type     string          `json:"type"` // history | message
	Messages []store.Message `json:"messages,omitempty"`
	Message  *store.Message  `json:"message,omitempty"`
}

// 客户端上行消息格式
type clientMsg struct {
	Content string `json:"content"`
}

type client struct {
	conn   *websocket.Conn
	send   chan serverMsg
	userID int64
}

type Hub struct {
	st     *store.Store
	origin string

	mu    sync.Mutex
	rooms map[int64]map[*client]struct{} // channelID -> 连接集合
}

func NewHub(st *store.Store, origin string) *Hub {
	return &Hub{st: st, origin: origin, rooms: make(map[int64]map[*client]struct{})}
}

// ServeHTTP 处理 GET /api/chat?channel=xx&token=xx（浏览器 WS 无法设请求头，token 走 query）。
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, devID, err := h.st.UserSessionByToken(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		http.Error(w, "登录已失效", http.StatusUnauthorized)
		return
	}
	// 与 auth 中间件同口径：绑定设备的会话（访客）须设备匹配；过期访客直接拒
	if devID != "" && devID != r.URL.Query().Get("device_id") {
		http.Error(w, "登录已失效", http.StatusUnauthorized)
		return
	}
	if u.Role == store.RoleGuest && u.ExpiresAt != nil && time.Now().After(*u.ExpiresAt) {
		http.Error(w, "登录已失效", http.StatusUnauthorized)
		return
	}
	channelName := r.URL.Query().Get("channel")
	ch, err := h.st.ChannelByName(r.Context(), channelName)
	if err != nil {
		http.Error(w, "频道不存在", http.StatusNotFound)
		return
	}
	// 进入权限：封禁 / 访客频道范围 / 邀请制白名单
	ok, reason, err := h.st.CanJoin(r.Context(), ch, u)
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}

	originPatterns := []string{h.origin}
	if h.origin == "*" {
		originPatterns = nil // 空表示不校验来源
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:     originPatterns,
		InsecureSkipVerify: h.origin == "*",
	})
	if err != nil {
		return
	}

	c := &client{conn: conn, send: make(chan serverMsg, 32), userID: u.ID}
	history, err := h.st.RecentMessages(r.Context(), ch.ID, historyLimit)
	if err == nil {
		c.send <- serverMsg{Type: "history", Messages: history}
	}
	h.join(ch.ID, c)
	defer h.leave(ch.ID, c)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go h.writeLoop(ctx, c)

	// 读循环：收到消息 -> 落库 -> 广播
	for {
		var m clientMsg
		err := wsjson.Read(ctx, conn, &m)
		if err != nil {
			return
		}
		m.Content = strings.TrimSpace(m.Content)
		if m.Content == "" || len(m.Content) > 2000 {
			continue
		}
		msg, err := h.st.AddMessage(ctx, ch.ID, u.ID, m.Content)
		if err != nil {
			log.Printf("消息落库失败: %v", err)
			continue
		}
		h.broadcast(ch.ID, serverMsg{Type: "message", Message: msg})
	}
}

func (h *Hub) writeLoop(ctx context.Context, c *client) {
	for {
		select {
		case <-ctx.Done():
			c.conn.Close(websocket.StatusNormalClosure, "bye")
			return
		case m := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := wsjson.Write(wctx, c.conn, m)
			cancel()
			if err != nil {
				c.conn.Close(websocket.StatusInternalError, "write error")
				return
			}
		}
	}
}

func (h *Hub) join(channelID int64, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[channelID] == nil {
		h.rooms[channelID] = make(map[*client]struct{})
	}
	h.rooms[channelID][c] = struct{}{}
}

func (h *Hub) leave(channelID int64, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rooms[channelID], c)
	if len(h.rooms[channelID]) == 0 {
		delete(h.rooms, channelID)
	}
}

// broadcast 非阻塞投递；塞满说明客户端消费不动，直接断开。
func (h *Hub) broadcast(channelID int64, m serverMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.rooms[channelID] {
		select {
		case c.send <- m:
		default:
			c.conn.Close(websocket.StatusGoingAway, "消费过慢")
		}
	}
}

// CloseChannel 断开某频道的全部聊天连接（频道被删除时调用）。
func (h *Hub) CloseChannel(channelID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.rooms[channelID] {
		c.conn.Close(websocket.StatusGoingAway, "频道已删除")
	}
}

// CloseUserChannel 断开某用户在某频道的全部聊天连接（踢出/封禁时调用）。
func (h *Hub) CloseUserChannel(userID, channelID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.rooms[channelID] {
		if c.userID == userID {
			c.conn.Close(websocket.StatusPolicyViolation, "被移出频道")
		}
	}
}
