package panel

import (
	"database/sql"
	"errors"
	"log"

	"golang.org/x/crypto/bcrypt"
)

// --- Admin / Auth ---

// EnsureAdmin 保证存在至少一个管理员（用 settings 中的 admin_user/admin_pass 初始化）。
func (db *DB) EnsureAdmin() error {
	var cnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM admins").Scan(&cnt); err != nil {
		return err
	}
	if cnt > 0 {
		return nil
	}
	user := db.GetSetting("admin_user")
	pass := db.GetSetting("admin_pass")
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.Exec("INSERT INTO admins(username,pass_hash) VALUES(?,?)", user, string(hash))
	if err != nil {
		return err
	}
	// 初始化后清空 settings 中的明文口令，避免两处不一致
	_ = db.SetSetting("admin_pass", "")
	log.Println("[db] 已创建初始管理员，用户名:", user, "(请登录后立即修改口令)")
	return nil
}

func (db *DB) VerifyPassword(username, password string) (bool, error) {
	var hash string
	err := db.QueryRow("SELECT pass_hash FROM admins WHERE username=?", username).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil, nil
}

// IsWeakPassword 判断管理员是否仍在使用初始口令 admin。
// 实机部署时面板可能直接暴露在公网，弱口令等于把内网整条隧道白送，必须强提示。
func (db *DB) IsWeakPassword() bool {
	user := db.GetSetting("admin_user")
	if user == "" {
		user = "admin"
	}
	ok, err := db.VerifyPassword(user, "admin")
	return err == nil && ok
}

func (db *DB) UpdatePassword(username, newPass string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.Exec("UPDATE admins SET pass_hash=? WHERE username=?", string(hash), username)
	return err
}
