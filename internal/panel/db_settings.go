package panel

import (
	"encoding/json"
)


// --- Settings ---

func (db *DB) GetSetting(key string) string {
	var v string
	_ = db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&v)
	return v
}


func (db *DB) SetSetting(key, value string) error {
	_, err := db.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}


func (db *DB) GetAllSettings() (map[string]string, error) {
	rows, err := db.Query("SELECT key,value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, nil
}


// MarshalSettingsJSON 便于调试导出。
func (db *DB) MarshalSettingsJSON() string {
	m, _ := db.GetAllSettings()
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b)
}
