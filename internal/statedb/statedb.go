// Package statedb opens the database disco-vm keeps its metadata in when it
// is not given one: a SQLite file in the state root.
package statedb

import (
	"path/filepath"

	"github.com/discobox-ai/x/gormdb"
	"gorm.io/gorm/logger"
)

// Name is the database's file name in a state root.
const Name = "disco-vm.db"

// Open opens (creating) the SQLite database in root. The caller closes it.
func Open(root string) (*gormdb.Pools, error) {
	return gormdb.Open(gormdb.Config{
		Driver: gormdb.DriverSQLite,
		DSN:    filepath.Join(root, Name),
		// A command line's stdout is its output, not a place for SQL.
		Logger: logger.Discard,
	})
}
