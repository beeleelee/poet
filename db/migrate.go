package db

import (
	"context"
	"fmt"
	"os"

	"github.com/syndtr/goleveldb/leveldb"
	"go.uber.org/zap"

	"github.com/spacemeshos/poet/logging"
)

// Temporary code to migrate a database to a new location.
// It opens both DBs and copies all the data from the old DB to the new one.
func Migrate(ctx context.Context, targetDbDir, oldDbDir string) error {
	log := logging.FromContext(ctx).With(zap.String("oldDbDir", oldDbDir), zap.String("targetDbDir", targetDbDir))
	if oldDbDir == targetDbDir {
		log.Debug("skipping in-place DB migration")
		return nil
	}
	if _, err := os.Stat(oldDbDir); os.IsNotExist(err) {
		log.Debug("skipping DB migration - old DB doesn't exist")
		return nil
	}
	log.Info("migrating DB location")

	oldDb, err := leveldb.OpenFile(oldDbDir, nil)
	if err != nil {
		return fmt.Errorf("opening old DB: %w", err)
	}
	defer oldDb.Close()

	targetDb, err := leveldb.OpenFile(targetDbDir, nil)
	if err != nil {
		return fmt.Errorf("opening target DB: %w", err)
	}
	defer targetDb.Close()

	trans, err := targetDb.OpenTransaction()
	if err != nil {
		return fmt.Errorf("opening new DB transaction: %w", err)
	}
	iter := oldDb.NewIterator(nil, nil)
	defer iter.Release()
	for iter.Next() {
		if err := trans.Put(iter.Key(), iter.Value(), nil); err != nil {
			trans.Discard()
			return fmt.Errorf("migrating key %X: %w", iter.Key(), err)
		}
	}
	iter.Release()
	if err := trans.Commit(); err != nil {
		return fmt.Errorf("committing DB transaction: %w", err)
	}

	// Remove old DB
	log.Info("removing the old DB")
	if err := oldDb.Close(); err != nil {
		return fmt.Errorf("closing old DB: %w", err)
	}
	if err := os.RemoveAll(oldDbDir); err != nil {
		return fmt.Errorf("removing old DB: %w", err)
	}
	log.Info("DB migrated to new location")
	return nil
}
