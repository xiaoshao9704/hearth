package store

import (
	"context"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	f := t.TempDir() + "/test.db"
	s, err := Open("sqlite://" + f)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProviderCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := &ProviderRecord{Alias: "lk-main", Type: "livekit",
		Params: map[string]string{"livekit_api_url": "http://127.0.0.1:7880", "livekit_api_secret": "s3cret"}}
	if err := s.CreateProvider(ctx, rec); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	got, err := s.ProviderByAlias(ctx, "lk-main")
	if err != nil || got.Type != "livekit" || got.Params["livekit_api_secret"] != "s3cret" {
		t.Fatalf("读回不一致: %+v err=%v", got, err)
	}
	if err := s.CreateProvider(ctx, rec); !IsUniqueViolation(err) {
		t.Fatalf("重复 alias 应唯一冲突，实际 %v", err)
	}
	if err := s.UpdateProviderParams(ctx, "lk-main", map[string]string{"livekit_api_url": "http://x:7880"}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	// 相同参数重复保存同样成功（存在性不靠 RowsAffected 推断）
	if err := s.UpdateProviderParams(ctx, "lk-main", map[string]string{"livekit_api_url": "http://x:7880"}); err != nil {
		t.Fatalf("相同参数重复保存应返回 nil: %v", err)
	}
	if err := s.UpdateProviderParams(ctx, "nope", map[string]string{"k": "v"}); err != ErrNotFound {
		t.Fatalf("更新不存在实例应 ErrNotFound，实际 %v", err)
	}
	got, _ = s.ProviderByAlias(ctx, "lk-main")
	if len(got.Params) != 1 || got.Params["livekit_api_url"] != "http://x:7880" {
		t.Fatalf("更新后 params 应整体替换: %+v", got.Params)
	}
	if _, err := s.ProviderByAlias(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("不存在应 ErrNotFound，实际 %v", err)
	}
	if err := s.DeleteProvider(ctx, "lk-main"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := s.ProviderByAlias(ctx, "lk-main"); err != ErrNotFound {
		t.Fatalf("删除后应 ErrNotFound，实际 %v", err)
	}
}
