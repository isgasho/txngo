package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
)

const (
	walPath = "./txngo.log"
	dbPath  = "./txngo.db"
	tmpPath = "./txngo.tmp"
)

const (
	LInsert = 1 + iota
	LDelete
	LUpdate
	LRead
	LCommit
	LAbort
)

var (
	ErrExist       = errors.New("record already exists")
	ErrNotExist    = errors.New("record not exists")
	ErrBufferShort = errors.New("buffer size is not enough to deserialize")
)

type RecordLog struct {
	Action uint8
	Record
}

func (r *RecordLog) Serialize(buf []byte) (int, error) {
	if len(buf) < 1 {
		return 0, ErrBufferShort
	}

	if r.Action > LRead {
		// LCommit or LAbort
		buf[0] = r.Action
		return 1, nil
	}

	// serialize record content first (check buffer size)
	n, err := r.Record.Serialize(buf[1:])
	if err != nil {
		return 0, err
	}

	buf[0] = r.Action
	return 1 + n, nil
}

func (r *RecordLog) Deserialize(buf []byte) (int, error) {
	if len(buf) < 1 {
		return 0, ErrBufferShort
	}
	r.Action = buf[0]

	switch r.Action {
	case LCommit:
		return 1, nil

	case LInsert, LUpdate, LDelete:
		n, err := r.Record.Deserialize(buf[1:])
		if err != nil {
			return 0, err
		}
		return 1 + n, nil

	default:
		return 0, fmt.Errorf("action is not supported : %v", r.Action)
	}
}

type Record struct {
	Key   string
	Value []byte
}

func (r *Record) Serialize(buf []byte) (int, error) {
	key := []byte(r.Key)
	value := r.Value
	total := 5 + len(key) + len(value)

	// check buffer size
	if len(buf) < total {
		return 0, ErrBufferShort
	}

	// serialize
	buf[0] = uint8(len(key))
	binary.BigEndian.PutUint32(buf[1:], uint32(len(r.Value)))
	copy(buf[5:], key)
	copy(buf[5+len(key):], r.Value)
	return total, nil
}

func (r *Record) Deserialize(buf []byte) (int, error) {
	if len(buf) < 5 {
		return 0, ErrBufferShort
	}

	// parse length
	keyLen := buf[0]
	valueLen := binary.BigEndian.Uint32(buf[1:])
	total := 5 + int(keyLen) + int(valueLen)
	if len(buf) < total {
		return 0, ErrBufferShort
	}

	// copy key and value from buffer
	r.Key = string(buf[5 : 5+keyLen])
	r.Value = make([]byte, valueLen)
	copy(r.Value, buf[5+keyLen:total])

	return total, nil
}

type Txn struct {
	wal      *os.File
	db       map[string]Record
	writeSet map[string]RecordLog
}

func NewTxn(wal *os.File) *Txn {
	return &Txn{
		wal:      wal,
		db:       make(map[string]Record),
		writeSet: make(map[string]RecordLog),
	}
}

func (txn *Txn) Read(key string) ([]byte, error) {
	if r, ok := txn.writeSet[key]; ok {
		if r.Action == LDelete {
			return nil, ErrNotExist
		}
		switch r.Action {
		case LInsert, LUpdate:
			return r.Value, nil
		default:
			return nil, fmt.Errorf("unexpected action in writeSet : %v", r.Action)
		}
	}

	r, ok := txn.db[key]
	if !ok {
		return nil, ErrNotExist
	}
	return r.Value, nil
}

func clone(v []byte) []byte {
	v2 := make([]byte, len(v))
	copy(v2, v)
	return v2
}

func (txn *Txn) Insert(key string, value []byte) error {
	// check writeSet
	if r, ok := txn.writeSet[key]; ok {
		if r.Action != LDelete {
			return ErrExist
		}
		// Delete -> Insert == Update
		// FIXME: Insert -> Delete -> Insert must be Insert (it is invalid)
		r.Action = LUpdate
		r.Value = clone(value)
		txn.writeSet[r.Key] = r
		return nil
	}

	// check that the key is not exists in db
	if _, ok := txn.db[key]; ok {
		return ErrExist
	}

	// reallocate string
	key = string(key)
	// clone value to prevent injection after transaction
	value = clone(value)

	// add insert action to writeSet
	txn.writeSet[key] = RecordLog{
		Action: LInsert,
		Record: Record{
			Key:   key,
			Value: value,
		},
	}
	return nil
}

func (txn *Txn) Update(key string, value []byte) error {
	// check writeSet
	if r, ok := txn.writeSet[key]; ok {
		if r.Action == LDelete {
			return ErrNotExist
		}
		r.Value = clone(value)
		txn.writeSet[r.Key] = r
		return nil
	}

	r, ok := txn.db[key]
	if !ok {
		return ErrNotExist
	}

	txn.writeSet[r.Key] = RecordLog{
		Action: LUpdate,
		Record: Record{
			Key: r.Key,
			// clone value to prevent injection after transaction
			Value: clone(value),
		},
	}
	return nil
}

func (txn *Txn) Delete(key string) error {
	// check writeSet
	if r, ok := txn.writeSet[key]; ok {
		if r.Action == LDelete {
			return ErrNotExist
		} else if r.Action == LInsert {
			delete(txn.writeSet, key)
			return nil
		}
		r.Value = nil
		// FIXME: Insert -> Delete will be only Delete (it is invalid)
		r.Action = LDelete
		txn.writeSet[r.Key] = r
		return nil
	}

	r, ok := txn.db[key]
	if !ok {
		return ErrNotExist
	}

	txn.writeSet[r.Key] = RecordLog{
		Action: LDelete,
		Record: Record{
			Key: r.Key,
		},
	}
	return nil
}

func (txn *Txn) Commit() error {
	if len(txn.writeSet) == 0 {
		// no need to write WAL
		return nil
	}
	var (
		i   int
		buf [4096]byte
	)

	for _, r := range txn.writeSet {
		// FIXME: key order in map will be randomized
		n, err := r.Serialize(buf[i:])
		if err == ErrBufferShort {
			// TODO: use writev
			return err
		}

		// TODO: delay write and combine multi log into one buffer
		_, err = txn.wal.Write(buf[:n])
		if err != nil {
			return err
		}
	}

	// write commit log
	n, err := (&RecordLog{Action: LCommit}).Serialize(buf[:])
	if err != nil {
		// commit log serialization must not fail
		log.Panic(err)
	}
	_, err = txn.wal.Write(buf[:n])
	if err != nil {
		return err
	}

	// sync this transaction
	err = txn.wal.Sync()
	if err != nil {
		return err
	}

	// write back writeSet to db (in memory)
	// FIXME: key order in map will be randomized
	for key, rlog := range txn.writeSet {
		switch rlog.Action {
		case LInsert:
			txn.db[rlog.Key] = rlog.Record

		case LUpdate:
			r := txn.db[rlog.Key]
			r.Value = rlog.Value
			txn.db[rlog.Key] = r

		case LDelete:
			delete(txn.db, rlog.Key)

		default:
			return fmt.Errorf("unexpected action : %v", rlog.Action)
		}

		// remove from writeSet
		delete(txn.writeSet, key)
	}

	return nil
}

func (txn *Txn) Abort() {
	for k := range txn.writeSet {
		delete(txn.writeSet, k)
	}
}

func main() {
	// execute on single thread
	runtime.GOMAXPROCS(1)

	wal, err := os.OpenFile(walPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		log.Panic(err)
	}
	//file, err := os.OpenFile(dbPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	//if err != nil {
	//	log.Panic(err)
	//}

	txn := NewTxn(wal)

	err = txn.Insert("key1", []byte("value1"))
	log.Println("insert key1", err)

	v, err := txn.Read("key1")
	log.Println("read key1", v, err)

	v, err = txn.Read("key2")
	log.Println("read key2", v, err)

	err = txn.Insert("key3", []byte("value3"))
	log.Println("insert key3", err)

	err = txn.Commit()
	log.Println("commit", err)

	err = txn.Insert("key1", []byte("value1"))
	log.Println("insert key1", err)

	v, err = txn.Read("key1")
	log.Println("read key1", v, err)

	v, err = txn.Read("key3")
	log.Println("read key3", v, err)

	log.Println("writeset", len(txn.writeSet), "db", len(txn.db))
}
