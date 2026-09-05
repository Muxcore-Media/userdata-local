package internal

import (
	"fmt"
	"strings"

	"github.com/Muxcore-Media/core/pkg/contracts"
)

func (m *Module) Settings() []contracts.SettingDef {
	return m.settingsDefs()
}

func (m *Module) UpdateSetting(key, value string) error {
	return m.updateSetting(key, value)
}

func (m *Module) settingsDefs() []contracts.SettingDef {
	m.cfgMu.RLock()
	dbPath := m.dbPath
	m.cfgMu.RUnlock()
	return []contracts.SettingDef{
		{
			Key:         "db_path",
			Label:       "Database path",
			Type:        contracts.SettingTypeString,
			Value:       dbPath,
			Default:     dbPath,
			Description: "SQLite path for per-user userdata blobs (USERDATA_LOCAL_DB_PATH)",
			Group:       "Storage",
		},
	}
}

func (m *Module) updateSetting(key, value string) error {
	value = strings.TrimSpace(value)
	switch key {
	case "db_path", "USERDATA_LOCAL_DB_PATH":
		if value == "" {
			return fmt.Errorf("db_path must not be empty")
		}
		m.cfgMu.Lock()
		m.dbPath = value
		m.cfgMu.Unlock()
		return nil
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
}
