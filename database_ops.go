package main

import (
	"database/sql"
	"strings"
	"time"
)

// ---- 活动计划 CRUD ----

func (db *DB) ListClaimPlans() ([]*ClaimPlan, error) {
	rows, err := db.conn.Query(`
		SELECT id, plan_name, cron_expr, is_active, target_type, account_id, account_group,
		       task_type, auto_pick, delay_seconds, last_run_at, last_run_status, last_run_msg,
		       created_at, updated_at
		FROM claim_plans ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ClaimPlan
	for rows.Next() {
		var p ClaimPlan
		var active, autoPick int
		if err := rows.Scan(&p.ID, &p.PlanName, &p.CronExpr, &active, &p.TargetType,
			&p.AccountID, &p.AccountGroup, &p.TaskType, &autoPick, &p.DelaySeconds,
			&p.LastRunAt, &p.LastRunStatus, &p.LastRunMsg, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.IsActive = active == 1
		p.AutoPick = autoPick == 1
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (db *DB) GetClaimPlan(id int64) (*ClaimPlan, error) {
	plans, err := db.ListClaimPlans()
	if err != nil {
		return nil, err
	}
	for _, p := range plans {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (db *DB) SaveClaimPlan(p *ClaimPlan) (int64, error) {
	if p.ID > 0 {
		_, err := db.conn.Exec(`
			UPDATE claim_plans SET plan_name=?, cron_expr=?, is_active=?, target_type=?,
			account_id=?, account_group=?, task_type=?, auto_pick=?, delay_seconds=?,
			updated_at=datetime('now','localtime') WHERE id=?`,
			p.PlanName, p.CronExpr, boolInt(p.IsActive), p.TargetType,
			p.AccountID, p.AccountGroup, p.TaskType, boolInt(p.AutoPick), p.DelaySeconds, p.ID)
		return p.ID, err
	}
	res, err := db.conn.Exec(`
		INSERT INTO claim_plans (plan_name, cron_expr, is_active, target_type,
			account_id, account_group, task_type, auto_pick, delay_seconds)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		p.PlanName, p.CronExpr, boolInt(p.IsActive), p.TargetType,
		p.AccountID, p.AccountGroup, p.TaskType, boolInt(p.AutoPick), p.DelaySeconds)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) DeleteClaimPlan(id int64) error {
	_, err := db.conn.Exec(`DELETE FROM claim_plans WHERE id = ?`, id)
	return err
}

func (db *DB) UpdateClaimPlanRun(id int64, status, msg string) error {
	_, err := db.conn.Exec(`
		UPDATE claim_plans SET last_run_at=datetime('now','localtime'),
		last_run_status=?, last_run_msg=? WHERE id=?`, status, msg, id)
	return err
}

// ---- 活动领取记录 ----

// HasResetRecordNear 是否已存在该账号 ±15 分钟内的成功重置记录
// （用于上游 used_at 去重：官方客户端等外部执行的重置不必重复入库）
// 注意：created_at 存的是 localtime 墙钟字符串，strftime('%s') 会按 UTC 解析，
// 需减去本地时区偏移才是真实 epoch。
func (db *DB) HasResetRecordNear(accountID int64, usedAtSec int64) (bool, error) {
	_, offset := time.Now().Zone()
	var n int
	err := db.conn.QueryRow(
		`SELECT COUNT(1) FROM claim_records
		 WHERE account_id=? AND task_type='reset' AND success=1
		   AND ABS(strftime('%s',created_at)-?-?)<900`,
		accountID, offset, usedAtSec).Scan(&n)
	return n > 0, err
}

func (db *DB) InsertClaimRecord(r *ClaimRecord) error {
	_, err := db.conn.Exec(`
		INSERT INTO claim_records (account_id, email, task_type, plan_id, plan_name, success, code, message, next_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		r.AccountID, r.Email, r.TaskType, r.PlanID, r.PlanName,
		boolInt(r.Success), r.Code, r.Message, r.NextAt)
	return err
}

func (db *DB) ListClaimRecords(limit int, accountID int64) ([]*ClaimRecord, error) {
	query := `SELECT id, created_at, account_id, email, task_type, plan_id, plan_name, success, code, message, next_at
		FROM claim_records`
	var args []interface{}
	if accountID > 0 {
		query += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ClaimRecord
	for rows.Next() {
		var r ClaimRecord
		var success int
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.AccountID, &r.Email, &r.TaskType,
			&r.PlanID, &r.PlanName, &success, &r.Code, &r.Message, &r.NextAt); err != nil {
			return nil, err
		}
		r.Success = success == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}

// ---- 使用记录 ----

func (db *DB) InsertUsageRecord(r *UsageRecord) error {
	_, err := db.conn.Exec(`
		INSERT INTO usage_records (account_id, email, model, prompt_tokens, completion_tokens,
			total_tokens, stream, status_code, duration_ms, ttft_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.AccountID, r.Email, r.Model, r.PromptTokens, r.CompletionTokens,
		r.TotalTokens, boolInt(r.Stream), r.StatusCode, r.DurationMs, r.TtftMs)
	return err
}

func (db *DB) ListUsageRecords(limit int) ([]*UsageRecord, error) {
	rows, err := db.conn.Query(`
		SELECT id, created_at, account_id, email, model, prompt_tokens, completion_tokens,
		       total_tokens, stream, status_code, duration_ms, ttft_ms
		FROM usage_records ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UsageRecord
	for rows.Next() {
		var r UsageRecord
		var stream int
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.AccountID, &r.Email, &r.Model,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &stream,
			&r.StatusCode, &r.DurationMs, &r.TtftMs); err != nil {
			return nil, err
		}
		r.Stream = stream == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}

// UsageStats 聚合统计（报表页）
func (db *DB) UsageStats(days int) (map[string]interface{}, error) {
	since := time.Now().AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
	out := map[string]interface{}{}
	row := db.conn.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
		       COALESCE(SUM(total_tokens),0), COALESCE(AVG(duration_ms),0), COALESCE(AVG(NULLIF(ttft_ms,0)),0)
		FROM usage_records WHERE created_at >= ?`, since)
	var n, pt, ct, tt int
	var avgDur, avgTtft float64
	if err := row.Scan(&n, &pt, &ct, &tt, &avgDur, &avgTtft); err != nil {
		return nil, err
	}
	out["requests"] = n
	out["prompt_tokens"] = pt
	out["completion_tokens"] = ct
	out["total_tokens"] = tt
	out["avg_duration_ms"] = int(avgDur)
	out["avg_ttft_ms"] = int(avgTtft)

	// 按模型分布
	models := map[string]int{}
	rows, err := db.conn.Query(`SELECT model, COUNT(*) FROM usage_records WHERE created_at >= ? GROUP BY model`, since)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var m string
			var c int
			if rows.Scan(&m, &c) == nil {
				models[m] = c
			}
		}
	}
	out["by_model"] = models

	// 按账号分布
	accounts := map[string]int{}
	rows2, err := db.conn.Query(`SELECT MAX(email), COUNT(*) FROM usage_records WHERE created_at >= ? GROUP BY account_id`, since)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var e string
			var c int
			if rows2.Scan(&e, &c) == nil {
				if e == "" {
					e = "unknown"
				}
				accounts[e] = c
			}
		}
	}
	out["by_account"] = accounts
	return out, nil
}

// ---- 代理节点 CRUD ----

func (db *DB) ListProxyNodes() ([]*ProxyNode, error) {
	rows, err := db.conn.Query(`
		SELECT id, name, type, host, port, username, password, is_default, group_name, enabled,
		       check_status, check_latency, check_ip, check_msg, check_at, created_at, updated_at
		FROM proxy_nodes ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProxyNode
	for rows.Next() {
		var n ProxyNode
		var isDef, enabled int
		if err := rows.Scan(&n.ID, &n.Name, &n.Type, &n.Host, &n.Port, &n.Username, &n.Password,
			&isDef, &n.GroupName, &enabled, &n.CheckStatus, &n.CheckLatency, &n.CheckIP,
			&n.CheckMsg, &n.CheckAt, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		n.IsDefault = isDef == 1
		n.Enabled = enabled == 1
		out = append(out, &n)
	}
	return out, rows.Err()
}

func (db *DB) SaveProxyNode(n *ProxyNode) (int64, error) {
	if n.IsDefault {
		db.conn.Exec(`UPDATE proxy_nodes SET is_default = 0`)
	}
	if n.ID > 0 {
		_, err := db.conn.Exec(`
			UPDATE proxy_nodes SET name=?, type=?, host=?, port=?, username=?, password=?,
			is_default=?, group_name=?, enabled=?, updated_at=datetime('now','localtime') WHERE id=?`,
			n.Name, n.Type, n.Host, n.Port, n.Username, n.Password,
			boolInt(n.IsDefault), n.GroupName, boolInt(n.Enabled), n.ID)
		return n.ID, err
	}
	res, err := db.conn.Exec(`
		INSERT INTO proxy_nodes (name, type, host, port, username, password, is_default, group_name, enabled)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		n.Name, n.Type, n.Host, n.Port, n.Username, n.Password,
		boolInt(n.IsDefault), n.GroupName, boolInt(n.Enabled))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) DeleteProxyNode(id int64) error {
	_, err := db.conn.Exec(`DELETE FROM proxy_nodes WHERE id = ?`, id)
	return err
}

func (db *DB) UpdateProxyNodeCheck(id int64, status string, latency int, ip, msg string) error {
	_, err := db.conn.Exec(`
		UPDATE proxy_nodes SET check_status=?, check_latency=?, check_ip=?, check_msg=?,
		check_at=datetime('now','localtime') WHERE id=?`, status, latency, ip, msg, id)
	return err
}

// ProxyNodeForGroup 查找组绑定的启用代理节点；组无绑定则回退默认节点。
// group_name 支持逗号分隔多组。
func (db *DB) ProxyNodeForGroup(group string) (*ProxyNode, error) {
	nodes, err := db.ListProxyNodes()
	if err != nil {
		return nil, err
	}
	if group != "" {
		for _, n := range nodes {
			if !n.Enabled {
				continue
			}
			for _, g := range strings.Split(n.GroupName, ",") {
				if strings.TrimSpace(g) == group {
					return n, nil
				}
			}
		}
	}
	for _, n := range nodes {
		if n.Enabled && n.IsDefault {
			return n, nil
		}
	}
	return nil, nil
}

// ---- 计划运行记录 ----

func (db *DB) InsertPlanRunRecord(r *PlanRunRecord) error {
	_, err := db.conn.Exec(`
		INSERT INTO plan_run_records (plan_id, plan_name, task_type, target_type, account_id,
			status, message, total, success_count, fail_count, duration_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		r.PlanID, r.PlanName, r.TaskType, r.TargetType, r.AccountID,
		r.Status, r.Message, r.Total, r.SuccessCount, r.FailCount, r.DurationMs)
	return err
}

func (db *DB) ListPlanRunRecords(limit int) ([]*PlanRunRecord, error) {
	rows, err := db.conn.Query(`
		SELECT id, plan_id, plan_name, task_type, target_type, account_id, run_at,
		       status, message, total, success_count, fail_count, duration_ms
		FROM plan_run_records ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PlanRunRecord
	for rows.Next() {
		var r PlanRunRecord
		if err := rows.Scan(&r.ID, &r.PlanID, &r.PlanName, &r.TaskType, &r.TargetType,
			&r.AccountID, &r.RunAt, &r.Status, &r.Message, &r.Total,
			&r.SuccessCount, &r.FailCount, &r.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
