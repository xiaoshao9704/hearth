package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"runtime"
	"testing"
	"time"

	"hearth/server/internal/config"
	"hearth/server/internal/tlscert"
)

func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	store := tlscert.New(t.TempDir()+"/tls", func(context.Context) tlscert.Settings {
		return tlscert.Settings{Source: tlscert.SourceSelf}
	})
	store.Check(context.Background())
	return store.TLSConfig()
}

func healthzHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	})
	return mux
}

func get(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestStartHTTPMerged 合并模式：同一个端口上明文与 TLS 都能拿到 /healthz，
// 关闭后 wait 正常返回、监听协程不泄漏。
func TestStartHTTPMerged(t *testing.T) {
	before := runtime.NumGoroutine()
	lis, err := startHTTP(config.Config{Addr: "127.0.0.1:0"}, healthzHandler(), testTLSConfig(t))
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	addr := lis.Addr.String()

	plain := &http.Client{Timeout: 5 * time.Second}
	secure := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2: true,
	}}
	if code := get(t, plain, "http://"+addr+"/healthz"); code != http.StatusOK {
		t.Fatalf("明文 /healthz 应 200，实际 %d", code)
	}
	if code := get(t, secure, "https://"+addr+"/healthz"); code != http.StatusOK {
		t.Fatalf("TLS /healthz 应 200，实际 %d", code)
	}

	done := make(chan error, 1)
	go func() { done <- lis.wait() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lis.shutdown(ctx)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅关闭不应报错: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("关闭后 cmux 的 Serve 没有返回")
	}

	plain.CloseIdleConnections()
	secure.CloseIdleConnections()
	assertNoLeak(t, before)
}

// TestStartHTTPSplit 分开模式：明文端口不接 TLS 握手，TLS 端口 200，关闭同样干净。
func TestStartHTTPSplit(t *testing.T) {
	before := runtime.NumGoroutine()
	// 两个地址必须不同才是分开模式（相同视为合并模式），https 侧先探一个空闲端口
	cfg := config.Config{Addr: "127.0.0.1:0", HTTPSAddr: freeAddr(t)}
	lis, err := startHTTP(cfg, healthzHandler(), testTLSConfig(t))
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	plain := &http.Client{Timeout: 5 * time.Second}
	secure := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	if code := get(t, plain, "http://"+lis.Addr.String()+"/healthz"); code != http.StatusOK {
		t.Fatalf("明文 /healthz 应 200，实际 %d", code)
	}
	if code := get(t, secure, "https://"+lis.TLSAddr.String()+"/healthz"); code != http.StatusOK {
		t.Fatalf("TLS /healthz 应 200，实际 %d", code)
	}
	// 明文端口上握 TLS 必须失败（分开模式不做嗅探）
	if _, err := secure.Get("https://" + lis.Addr.String() + "/healthz"); err == nil {
		t.Fatal("分开模式下明文端口不该接受 TLS 握手")
	}

	done := make(chan error, 1)
	go func() { done <- lis.wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lis.shutdown(ctx)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅关闭不应报错: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("关闭后监听协程没有返回")
	}
	plain.CloseIdleConnections()
	secure.CloseIdleConnections()
	assertNoLeak(t, before)
}

// freeAddr 探一个当前空闲的端口（分开模式两个地址不能都写 :0——那样两串相同，按合并模式走）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// assertNoLeak 协程数回到基线（关闭后连接收尾有先后，给 2 秒窗口）。
func assertNoLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		now := runtime.NumGoroutine()
		if now <= before+1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("协程泄漏: 关闭前 %d，关闭后 %d", before, now)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
