package oplog

import (
	"errors"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	v1 "github.com/garethgeorge/resticui/gen/go/v1"
	"github.com/garethgeorge/resticui/internal/database/indexutil"
	"github.com/garethgeorge/resticui/internal/database/serializationutil"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)


type EventType int

const (
	EventTypeUnknown = EventType(iota)
	EventTypeOpCreated = EventType(iota)
	EventTypeOpUpdated = EventType(iota)
)

var (
	SystemBucket = []byte("oplog.system") // system stores metadata
	OpLogBucket  = []byte("oplog.log") // oplog stores the operations themselves
	RepoIndexBucket = []byte("oplog.repo_idx") // repo_index tracks IDs of operations affecting a given repo
	PlanIndexBucket = []byte("oplog.plan_idx") // plan_index tracks IDs of operations affecting a given plan
	IndexedSnapshotsSetBucket = []byte("oplog.indexed_snapshots") // indexed_snapshots is a set of snapshot IDs that have been indexed
)

// OpLog represents a log of operations performed.
// Operations are indexed by repo and plan.
type OpLog struct {
	db *bolt.DB

	subscribersMu sync.RWMutex
	subscribers []*func(EventType, *v1.Operation)
}

func NewOpLog(databasePath string) (*OpLog, error) {
	if err := os.MkdirAll(path.Dir(databasePath), 0700); err != nil {
		return nil, fmt.Errorf("error creating database directory: %s", err)
	}

	db, err := bolt.Open(databasePath, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("error opening database: %s", err)
	}

	// Create the buckets if they don't exist
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{
			SystemBucket, OpLogBucket, RepoIndexBucket, PlanIndexBucket, IndexedSnapshotsSetBucket,
		} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("error creating bucket %s: %s", string(bucket), err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &OpLog{db: db}, nil
}

func (o *OpLog) Close() error {
	return o.db.Close()
}

// Add adds a generic operation to the operation log.
func (o *OpLog) Add(op *v1.Operation) error {
	if op.Id != 0 {
		return errors.New("operation already has an ID, OpLog.Add is expected to set the ID")
	}

	err := o.db.Update(func(tx *bolt.Tx) error {
		err := o.addOperationHelper(tx, op)
		if err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		o.notifyHelper(EventTypeOpCreated, op)
	}

	return err
}

func (o *OpLog) BulkAdd(ops []*v1.Operation) {
	o.db.Update(func(tx *bolt.Tx) error {
		for _, op := range ops {
			if err := o.addOperationHelper(tx, op); err != nil {
				return err
			}
		}
		return nil
	})
}

func (o *OpLog) addOperationHelper(tx *bolt.Tx, op *v1.Operation) error {
	b := tx.Bucket(OpLogBucket)

	id, err := b.NextSequence()
	if err != nil {
		return fmt.Errorf("error getting next sequence: %w", err)
	}

	op.Id = int64(id)

	bytes, err := proto.Marshal(op)
	if err != nil {
		return fmt.Errorf("error marshalling operation: %w", err)
	}


	if err := b.Put(serializationutil.Itob(op.Id), bytes); err != nil {
		return fmt.Errorf("error putting operation into bucket: %w", err)
	}

	// Update always universal indices
	if op.RepoId != "" {
		if err := indexutil.IndexByteValue(tx.Bucket(RepoIndexBucket), []byte(op.RepoId), op.Id); err != nil {
			return fmt.Errorf("error adding operation to repo index: %w", err)
		}
	}
	if op.PlanId != "" {
		if err := indexutil.IndexByteValue(tx.Bucket(PlanIndexBucket), []byte(op.PlanId), op.Id); err != nil {
			return fmt.Errorf("error adding operation to repo index: %w", err)
		}
	}

	// Update operation type dependent indices.
	switch wrappedOp := op.Op.(type) {
	case *v1.Operation_OperationBackup:
		// Nothing extra to be done.
	case *v1.Operation_OperationIndexSnapshot:
		if wrappedOp.OperationIndexSnapshot == nil || wrappedOp.OperationIndexSnapshot.Snapshot == nil {
			return errors.New("op.OperationIndexSnapshot or op.OperationIndexSnapshot.Snapshot is nil")
		}
		snapshotId := serializationutil.NormalizeSnapshotId(wrappedOp.OperationIndexSnapshot.Snapshot.Id)
		key := serializationutil.BytesToKey([]byte(snapshotId))
		if err := tx.Bucket(IndexedSnapshotsSetBucket).Put(key, serializationutil.Itob(op.Id)); err != nil {
			return fmt.Errorf("error adding OperationIndexSnapshot to indexed snapshots set: %w", err)
		}
	default:
		return fmt.Errorf("unknown operation type: %T", wrappedOp)
	}

	return nil
}

func (o *OpLog) HasIndexedSnapshot(snapshotId string) (int64, error) {
	var id int64
	if err := o.db.View(func(tx *bolt.Tx) error {
		key := serializationutil.BytesToKey([]byte(snapshotId))
		idBytes := tx.Bucket(IndexedSnapshotsSetBucket).Get(key)
		if idBytes == nil {
			id = -1
		} else {
			id = serializationutil.Btoi(idBytes)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return id, nil
}

func (o *OpLog) Update(op *v1.Operation) error {
	if op.Id == 0 {
		return errors.New("operation does not have an ID, OpLog.Update expects operation with an ID")
	}

	err := o.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(OpLogBucket)

		if b.Get(serializationutil.Itob(op.Id)) == nil {
			return fmt.Errorf("operation with ID %d does not exist", op.Id)
		}

		bytes, err := proto.Marshal(op)
		if err != nil {
			return fmt.Errorf("error marshalling operation: %w", err)
		}

		if err := b.Put(serializationutil.Itob(op.Id), bytes); err != nil {
			return fmt.Errorf("error putting operation into bucket: %w", err)
		}

		return nil
	})
	if err == nil {
		o.notifyHelper(EventTypeOpUpdated, op)
	}
	return err
}

func (o *OpLog) notifyHelper(eventType EventType, op *v1.Operation) {
	o.subscribersMu.RLock()
	defer o.subscribersMu.RUnlock()
	for _, sub := range o.subscribers {
		(*sub)(eventType, op)
	}
}

func (o *OpLog) getHelper(b *bolt.Bucket, id int64) (*v1.Operation, error) {
	bytes := b.Get(serializationutil.Itob(id))
	if bytes == nil {
		return nil, fmt.Errorf("operation with ID %d does not exist", id)
	}

	var op v1.Operation
	if err := proto.Unmarshal(bytes, &op); err != nil {
		return nil, fmt.Errorf("error unmarshalling operation: %w", err)
	}

	return &op, nil
}

func (o *OpLog) Get(id int64) (*v1.Operation, error) {
	var op *v1.Operation
	if err := o.db.View(func(tx *bolt.Tx) error {
		var err error
		op, err = o.getHelper(tx.Bucket(OpLogBucket), id)
		return err 
	}); err != nil {
		return nil, err
	}
	return op, nil
}

func (o *OpLog) GetByRepo(repoId string, filter Filter) ([]*v1.Operation, error) {
	var ops []*v1.Operation
	if err := o.db.View(func(tx *bolt.Tx) error {
		ids := indexutil.IndexSearchByteValue(tx.Bucket(RepoIndexBucket), []byte(repoId)).ToSlice()

		b := tx.Bucket(OpLogBucket)
		for _, id := range ids {
			op, err := o.getHelper(b, id)
			if err != nil {
				return err
			}
			ops = append(ops, op)
		}

		return nil
	}); err != nil {
		return nil, err
	}
	return ops, nil
}

func (o *OpLog) GetByPlan(planId string, filter Filter) ([]*v1.Operation, error) {
	var ops []*v1.Operation
	if err := o.db.View(func(tx *bolt.Tx) error {
		ids := indexutil.IndexSearchByteValue(tx.Bucket(PlanIndexBucket), []byte(planId)).ToSlice()

		b := tx.Bucket(OpLogBucket)
		for _, id := range ids {
			op, err := o.getHelper(b, id)
			if err != nil {
				return err
			}
			ops = append(ops, op)
		}

		return nil
	}); err != nil {
		return nil, err
	}
	return ops, nil
}

func (o *OpLog) Subscribe(callback *func(EventType, *v1.Operation)) {
	o.subscribersMu.Lock()
	defer o.subscribersMu.Unlock()	
	o.subscribers = append(o.subscribers, callback)
}

func (o *OpLog) Unsubscribe(callback *func(EventType, *v1.Operation)) {
	o.subscribersMu.Lock()
	defer o.subscribersMu.Unlock()
	subs := o.subscribers
	for i, c := range subs {
		if c == callback {
			subs[i] = subs[len(subs) - 1]
			o.subscribers = subs[:len(o.subscribers) - 1]
		}
	}
}

type Filter func([]int64)[]int64

func FilterKeepAll() Filter {
	return func(ids []int64) []int64 {
		return ids
	}
}

func FilterLastN(n int64) Filter {
	return func(ids []int64) []int64 {
		if len(ids) > int(n) {
			ids = ids[len(ids)-int(n):]
		}
		return ids
	}
}

func FilterLimitOffset(limit, offset int64) Filter {
	return func(ids []int64) []int64 {
		if len(ids) > int(offset) {
			ids = ids[offset:]
		}
		if len(ids) > int(limit) {
			ids = ids[:limit]
		}
		return ids
	}
}