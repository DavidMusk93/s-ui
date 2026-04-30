package database

import (
	"encoding/json"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/alireza0/s-ui/config"
	"github.com/alireza0/s-ui/database/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// db holds the singleton *gorm.DB.
//
// ROOT CAUSE of FD leak: gorm.Open creates a *sql.DB pool. Simply
// overwriting db orphans the old pool; *sql.DB is NOT garbage-collected,
// so its SQLite FDs (db, -wal, -shm) leak forever. Always close the
// old pool via CloseDB() or ReopenDB() before reassignment.
var db *gorm.DB

var dbMutex sync.Mutex
var dbInitOnce sync.Once

func initUser() error {
	var count int64
	err := db.Model(&model.User{}).Count(&count).Error
	if err != nil {
		return err
	}
	if count == 0 {
		user := &model.User{
			Username: "admin",
			Password: "admin",
		}
		return db.Create(user).Error
	}
	return nil
}

// OpenDB creates a new SQLite pool. It does NOT close an existing pool.
// CAUTION: Callers replacing the DB must CloseDB() first or use ReopenDB().
func OpenDB(dbPath string) error {
	dir := path.Dir(dbPath)
	err := os.MkdirAll(dir, 01740)
	if err != nil {
		return err
	}

	var gormLogger logger.Interface

	if config.IsDebug() {
		gormLogger = logger.Default
	} else {
		gormLogger = logger.Discard
	}

	c := &gorm.Config{
		Logger: gormLogger,
	}
	sep := "?"
	if strings.Contains(dbPath, "?") {
		sep = "&"
	}
	dsn := dbPath + sep + "_busy_timeout=10000&_journal_mode=WAL"
	db, err = gorm.Open(sqlite.Open(dsn), c)
	if err != nil {
		return err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if config.IsDebug() {
		db = db.Debug()
	}
	return nil
}

// InitDB opens the DB once and runs schema migrations once.
// Safe to call multiple times; ReopenDB() should be used for import.
func InitDB(dbPath string) error {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	if db == nil {
		if err := OpenDB(dbPath); err != nil {
			return err
		}
	}

	var initErr error
	dbInitOnce.Do(func() {
		// Default Outbounds
		if !db.Migrator().HasTable(&model.Outbound{}) {
			db.Migrator().CreateTable(&model.Outbound{})
			defaultOutbound := []model.Outbound{
				{Type: "direct", Tag: "direct", Options: json.RawMessage(`{}`)},
			}
			db.Create(&defaultOutbound)
		}

		initErr = db.AutoMigrate(
			&model.Setting{},
			&model.Tls{},
			&model.Inbound{},
			&model.Outbound{},
			&model.Service{},
			&model.Endpoint{},
			&model.User{},
			&model.Tokens{},
			&model.Stats{},
			&model.Client{},
			&model.Changes{},
		)
		if initErr != nil {
			return
		}
		initErr = initUser()
	})

	return initErr
}

func GetDB() *gorm.DB {
	return db
}

// CloseDB closes the pool and releases SQLite FDs (db, -wal, -shm).
func CloseDB() error {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	db = nil
	return sqlDB.Close()
}

// ReopenDB closes the old pool then opens a new one.
// Use this during DB import to avoid FD accumulation.
func ReopenDB(dbPath string) error {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	if db != nil {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	return OpenDB(dbPath)
}

func IsNotFound(err error) bool {
	return err == gorm.ErrRecordNotFound
}
