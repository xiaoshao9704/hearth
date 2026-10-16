// 存储层：用户、会话、频道、聊天消息、设备、推流令牌、封禁、白名单。
// 多后端：DATABASE_URL 决定方言 —— 空/file:/sqlite: 路径 = sqlite（modernc.org/sqlite，无 cgo）；
// mysql:// = go-sql-driver/mysql；postgres:// = jackc/pgx/v5。三种驱动均为纯 Go，可交叉编译。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/migrate"
	"github.com/uptrace/bun/schema"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("记录不存在")

// ---- 方言 ----

type dialect struct {
	name string // sqlite | mysql | postgres
}

// parseDBURL 解析 DATABASE_URL，返回方言、驱动名与 DSN。
func parseDBURL(dbURL string) (dialect, string, string, error) {
	switch {
	case strings.HasPrefix(dbURL, "mysql://"):
		u, err := url.Parse(dbURL)
		if err != nil {
			return dialect{}, "", "", fmt.Errorf("DATABASE_URL 解析失败: %w", err)
		}
		auth := u.User.Username()
		if pw, ok := u.User.Password(); ok {
			auth += ":" + pw
		}
		// parseTime=true：DATETIME 直接扫成 time.Time；
		// clientFoundRows=true：RowsAffected 统一为「匹配行数」（默认只算实际改变的行，
		// 同值 UPDATE 会误报 0，靠 RowsAffected 判存在的逻辑在 MySQL 下会误判）
		dsn := fmt.Sprintf("%s@tcp(%s)%s?parseTime=true&clientFoundRows=true", auth, u.Host, u.Path)
		return dialect{"mysql"}, "mysql", dsn, nil
	case strings.HasPrefix(dbURL, "postgres://"), strings.HasPrefix(dbURL, "postgresql://"):
		return dialect{"postgres"}, "pgx", dbURL, nil
	default:
		// 空 / file: / sqlite: / 裸路径 都按 sqlite 处理
		dsn := strings.TrimPrefix(dbURL, "sqlite:")
		if dsn == "" {
			dsn = "hearth.db"
		}
		if !strings.Contains(dsn, "?") {
			dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
		}
		return dialect{"sqlite"}, "sqlite", dsn, nil
	}
}

type Store struct {
	bun *bun.DB // Bun 句柄（包在 *sql.DB 上），全部查询与 schema 迁移走它
	d   dialect
}

// bunDialect 把方言名映射到 Bun 方言实现（驱动仍用原有三个纯 Go 驱动）。
func (d dialect) bunDialect() schema.Dialect {
	switch d.name {
	case "mysql":
		return mysqldialect.New()
	case "postgres":
		return pgdialect.New()
	default:
		return sqlitedialect.New()
	}
}

func Open(dbURL string) (*Store, error) {
	d, driver, dsn, err := parseDBURL(dbURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库连接失败: %w", err)
	}
	bunDB := bun.NewDB(db, d.bunDialect())
	s := &Store{bun: bunDB, d: d}
	// schema 迁移失败即启动失败（与旧 migrate() 语义一致）。
	// WithMarkAppliedOnSuccess：Up 成功后才登记 bun_migrations——默认是先登记再执行，
	// 中途失败会被永久跳过、库停在半迁移态；baselineUp 幂等，开此选项后重试安全。
	migrator := migrate.NewMigrator(bunDB, Migrations, migrate.WithMarkAppliedOnSuccess(true))
	ctx := context.Background()
	if err := migrator.Init(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema 迁移初始化失败: %w", err)
	}
	if _, err := migrator.Migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema 迁移失败: %w", err)
	}
	return s, nil
}

// Close 关闭底层连接（bun.Close 会关掉包在里面的 *sql.DB）。
func (s *Store) Close() error { return s.bun.Close() }

// isDuplicateErr 判"列/索引已存在"类错误（三方言文案不同），baseline 迁移吞掉建重错误。
func isDuplicateErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate key name") // mysql CREATE INDEX 无 IF NOT EXISTS
}

// isMissingTableErr 判"表不存在"（三方言文案不同），游标 v2 重入时旧 ingresses 表已删属正常。
func isMissingTableErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") || // sqlite
		strings.Contains(msg, "doesn't exist") || // mysql
		strings.Contains(msg, "does not exist") // postgres
}

// IsUniqueViolation 判断唯一约束冲突（用户名/频道名重复），覆盖三种方言的错误文案。
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint failed") || // sqlite
		strings.Contains(msg, "duplicate entry") || // mysql
		strings.Contains(msg, "duplicate key") // postgres
}
