package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	clientLogBodyLimit = 8 << 10
	clientLogPerMinute = 120
)

type clientLogRate struct {
	started time.Time
	count   int
}

type clientLogRequest struct {
	Level        string `json:"level"`
	Event        string `json:"event"`
	Session      string `json:"session,omitempty"`
	Channel      string `json:"channel,omitempty"`
	Role         string `json:"role,omitempty"`
	Engine       string `json:"engine,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	ElapsedMS    int64  `json:"elapsed_ms,omitempty"`
	Online       bool   `json:"online"`
	Visibility   string `json:"visibility,omitempty"`
	Network      string `json:"network,omitempty"`
	State        string `json:"state,omitempty"`
	Reason       string `json:"reason,omitempty"`
	ErrorName    string `json:"error_name,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

var (
	clientLogEventRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	clientLogJWTRE    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_-]{10,})?\b`)
	clientLogURLRE    = regexp.MustCompile(`(?i)\b((?:https?|wss?)://[^\s?]+)\?[^\s]+`)
	clientLogKeyRE    = regexp.MustCompile(`(?i)((?:access[_-]?token|token|secret|signature|authorization|api[_-]?key|key)=)[^&\s]+`)
	clientLogBearerRE = regexp.MustCompile(`(?i)\bBearer\s+[^\s]+`)
)

func truncateUTF8(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func redactClientLogText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = clientLogBearerRE.ReplaceAllString(s, "Bearer <redacted>")
	s = clientLogJWTRE.ReplaceAllString(s, "<token>")
	s = clientLogURLRE.ReplaceAllString(s, "$1?<redacted>")
	s = clientLogKeyRE.ReplaceAllString(s, "$1<redacted>")
	return truncateUTF8(s, n)
}

func (a *API) allowClientLog(userID int64, now time.Time) bool {
	a.clientLogMu.Lock()
	defer a.clientLogMu.Unlock()
	if a.clientLogRates == nil {
		a.clientLogRates = make(map[int64]clientLogRate)
	}
	rate := a.clientLogRates[userID]
	if rate.started.IsZero() || now.Sub(rate.started) >= time.Minute {
		rate = clientLogRate{started: now}
	}
	if rate.count >= clientLogPerMinute {
		return false
	}
	rate.count++
	a.clientLogRates[userID] = rate
	if len(a.clientLogRates) > 1024 {
		for id, entry := range a.clientLogRates {
			if now.Sub(entry.started) >= 2*time.Minute {
				delete(a.clientLogRates, id)
			}
		}
	}
	return true
}

func (a *API) clientLog(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, clientLogBodyLimit)
	dec := json.NewDecoder(r.Body)
	var in clientLogRequest
	if err := dec.Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "诊断内容过大")
			return
		}
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if in.Level != "info" && in.Level != "warn" && in.Level != "error" {
		writeErr(w, http.StatusBadRequest, "诊断级别无效")
		return
	}
	if !clientLogEventRE.MatchString(in.Event) {
		writeErr(w, http.StatusBadRequest, "诊断事件无效")
		return
	}
	if in.Role != "" && in.Role != "voice" && in.Role != "stage" {
		writeErr(w, http.StatusBadRequest, "诊断线路无效")
		return
	}
	if in.Attempt < 0 || in.Attempt > 1000 || in.ElapsedMS < 0 || in.ElapsedMS > int64((24*time.Hour)/time.Millisecond) {
		writeErr(w, http.StatusBadRequest, "诊断数值无效")
		return
	}
	u := userFrom(r)
	if !a.allowClientLog(u.ID, time.Now()) {
		writeErr(w, http.StatusTooManyRequests, "诊断上报过于频繁")
		return
	}
	log.Printf("前端诊断: uid=%d username=%q user_agent=%q level=%q event=%q session=%q channel=%q role=%q engine=%q endpoint=%q attempt=%d elapsed_ms=%d online=%t visibility=%q network=%q state=%q reason=%q error_name=%q error=%q",
		u.ID,
		redactClientLogText(u.Username, 80),
		redactClientLogText(r.UserAgent(), 300),
		in.Level,
		in.Event,
		redactClientLogText(in.Session, 40),
		redactClientLogText(in.Channel, 120),
		in.Role,
		redactClientLogText(in.Engine, 40),
		redactClientLogText(in.Endpoint, 200),
		in.Attempt,
		in.ElapsedMS,
		in.Online,
		redactClientLogText(in.Visibility, 24),
		redactClientLogText(in.Network, 80),
		redactClientLogText(in.State, 80),
		redactClientLogText(in.Reason, 120),
		redactClientLogText(in.ErrorName, 80),
		redactClientLogText(in.ErrorMessage, 600),
	)
	w.WriteHeader(http.StatusNoContent)
}
