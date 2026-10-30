package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// systemLogRetention 是运行日志的保留窗口。
// 与账号表不同，日志表只按时间与条数收敛，不随账号数量增长。
const (
	systemLogRetention   = 7 * 24 * time.Hour
	systemLogMaxEntries  = 10000
	systemLogQueryWindow = 7 * 24 * time.Hour
)

// 运行日志**故意**不设账号外键：账号被删除后，它的诊断时间线必须留下来——
// 排查「这个号为什么掉线」时，恰恰是账号已经不在了才最需要看历史。
// 表里也永不存储账号身份（昵称、openid）、凭据、扫码链接或原始错误文本。
const systemLogSchema = `CREATE TABLE IF NOT EXISTS system_logs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 created_at INTEGER NOT NULL,
 level TEXT NOT NULL,
 category TEXT NOT NULL,
 event TEXT NOT NULL,
 message TEXT NOT NULL,
 account_id INTEGER NOT NULL DEFAULT 0,
 channel TEXT NOT NULL DEFAULT '',
 details TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_system_logs_category ON system_logs(category,id);
CREATE INDEX IF NOT EXISTS idx_system_logs_level ON system_logs(level,id);
CREATE INDEX IF NOT EXISTS idx_system_logs_account ON system_logs(account_id,id);
CREATE INDEX IF NOT EXISTS idx_system_logs_created ON system_logs(created_at);`

// SystemLog 是一条运行日志。Details 只含结构化白名单字段。
type SystemLog struct {
	ID        int64           `json:"id"`
	CreatedAt int64           `json:"created_at"`
	Level     string          `json:"level"`
	Category  string          `json:"category"`
	Event     string          `json:"event"`
	Message   string          `json:"message"`
	AccountID int64           `json:"account_id,omitempty"`
	Channel   string          `json:"channel,omitempty"`
	Details   json.RawMessage `json:"details"`
}

// SystemLogFilter 是运行日志的查询条件，全部为可选。
type SystemLogFilter struct {
	Category, Level, Channel string
	AccountID, Before, Since int64
	Limit                    int
}

func (db *DB) InitSystemLogs(ctx context.Context) error {
	_, err := db.sql.ExecContext(ctx, systemLogSchema)
	return err
}

// AppendSystemLogs 批量写入日志并就地裁剪。
//
// 时间与条数两个上限都在同一个事务里生效，因此重启不会让表继续膨胀；
// 上限取的是「当前最大 ID 往前 10000 条」，而不是 COUNT，避免全表扫描。
func (db *DB) AppendSystemLogs(ctx context.Context, entries []SystemLog) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range entries {
		if _, err = tx.ExecContext(ctx, `INSERT INTO system_logs(created_at,level,category,event,message,account_id,channel,details) VALUES(?,?,?,?,?,?,?,?)`,
			item.CreatedAt, item.Level, item.Category, item.Event, item.Message, item.AccountID, item.Channel, string(item.Details)); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM system_logs WHERE created_at < ? OR id <= COALESCE((SELECT id FROM system_logs ORDER BY id DESC LIMIT 1 OFFSET ?),0)`,
		time.Now().Add(-systemLogRetention).UnixMilli(), systemLogMaxEntries); err != nil {
		return err
	}
	return tx.Commit()
}

// ListSystemLogs 按 ID 倒序查询日志。Limit 必须在 1..200 之间（分页会多取一条）。
func (db *DB) ListSystemLogs(ctx context.Context, f SystemLogFilter) ([]SystemLog, error) {
	where, args := []string{"created_at>=?"}, []any{time.Now().Add(-systemLogQueryWindow).UnixMilli()}
	for _, pair := range [][2]string{{"category", f.Category}, {"level", f.Level}, {"channel", f.Channel}} {
		if pair[1] != "" {
			where = append(where, pair[0]+"=?")
			args = append(args, pair[1])
		}
	}
	for _, part := range []struct {
		sql   string
		value int64
	}{{"account_id=?", f.AccountID}, {"id<?", f.Before}, {"created_at>=?", f.Since}} {
		if part.value > 0 {
			where = append(where, part.sql)
			args = append(args, part.value)
		}
	}
	if f.Limit < 1 || f.Limit > 200 {
		return nil, fmt.Errorf("invalid log limit")
	}
	args = append(args, f.Limit)
	rows, err := db.sql.QueryContext(ctx, `SELECT id,created_at,level,category,event,message,account_id,channel,details FROM system_logs WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SystemLog, 0)
	for rows.Next() {
		var item SystemLog
		var details string
		if err := rows.Scan(&item.ID, &item.CreatedAt, &item.Level, &item.Category, &item.Event, &item.Message, &item.AccountID, &item.Channel, &details); err != nil {
			return nil, err
		}
		item.Details = json.RawMessage(details)
		out = append(out, item)
	}
	return out, rows.Err()
}
