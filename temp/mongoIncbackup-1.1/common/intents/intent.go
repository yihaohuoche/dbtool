// Package intents provides utilities for performing backup/restore operations.
package intents

import (
	"io"
	"mongoIncbackup-1.1/common/log"
	"mongoIncbackup-1.1/mgo.v2/bson"
	"sync"
)

type file interface {
	io.ReadWriteCloser
	Open() error
	Pos() int64
}

// FileNeedsIOBuffer is an interface that denotes that a struct needs
// an IO buffer that is managed by an outside control. This interface
// is used to both hand off a buffer to a struct and signal that it should
// release its buffer. Added to reduce memory usage as outlined in TOOLS-1088.
type FileNeedsIOBuffer interface {
	TakeIOBuffer([]byte)
	ReleaseIOBuffer()
}

// mongorestore first scans the directory to generate a list
// of all files to restore and what they map to. TODO comments
type Intent struct {
	// Namespace info
	DB string
	C  string

	// File locations as absolute paths
	BSONFile     file
	BSONSize     int64
	MetadataFile file

	// Indicates where the intent will be read from or written to
	Location         string
	MetadataLocation string

	// Collection options
	Options *bson.D

	// File/collection size, for some prioritizer implementations.
	// Units don't matter as long as they are consistent for a given use case.
	Size int64
}

func (it *Intent) Namespace() string {
	return it.DB + "." + it.C
}

func (it *Intent) IsOplog() bool {
	return it.DB == "" && it.C == "oplog"
}

func (it *Intent) IsUsers() bool {
	if it.C == "$admin.system.users" {
		return true
	}
	if it.DB == "admin" && it.C == "system.users" {
		return true
	}
	return false
}

func (it *Intent) IsRoles() bool {
	if it.C == "$admin.system.roles" {
		return true
	}
	if it.DB == "admin" && it.C == "system.roles" {
		return true
	}
	return false
}

func (it *Intent) IsAuthVersion() bool {
	if it.C == "$admin.system.version" {
		return true
	}
	if it.DB == "admin" && it.C == "system.version" {
		return true
	}
	return false
}

func (it *Intent) IsSystemIndexes() bool {
	return it.C == "system.indexes" && it.BSONFile != nil
}

func (intent *Intent) IsSpecialCollection() bool {
	return intent.IsSystemIndexes() || intent.IsUsers() || intent.IsRoles() || intent.IsAuthVersion()
}

type Manager struct {
	// intents are for all of the regular user created collections
	intents map[string]*Intent
	// special intents are for all of the collections that are created by mongod
	// and require special handling
	specialIntents map[string]*Intent

	// legacy mongorestore works in the order that paths are discovered,
	// so we need an ordered data structure to preserve this behavior.
	intentsByDiscoveryOrder []*Intent

	// we need different scheduling order depending on the target
	// mongod/mongos and whether or not we are multi threading;
	// the IntentPrioritizer interface encapsulates this.
	prioritizer     IntentPrioritizer
	priotitizerLock *sync.Mutex

	// special cases that should be saved but not be part of the queue.
	// used to deal with oplog and user/roles restoration, which are
	// handled outside of the basic logic of the tool
	oplogIntent   *Intent
	usersIntent   *Intent
	rolesIntent   *Intent
	versionIntent *Intent
	indexIntents  map[string]*Intent
}

func NewIntentManager() *Manager {
	return &Manager{
		intents:                 map[string]*Intent{},
		specialIntents:          map[string]*Intent{},
		intentsByDiscoveryOrder: []*Intent{},
		priotitizerLock:         &sync.Mutex{},
		indexIntents:            map[string]*Intent{},
	}
}

// HasConfigDBIntent returns a bool indicating if any of the intents refer to the "config" database.
// This can be used to check for possible unwanted conflicts before restoring to a sharded system.
func (mgr *Manager) HasConfigDBIntent() bool {
	for _, intent := range mgr.intentsByDiscoveryOrder {
		if intent.DB == "config" {
			return true
		}
	}
	return false
}

// Put inserts an intent into the manager. Intents for the same collection
// are merged together, so that BSON and metadata files for the same collection
// are returned in the same intent.
func (manager *Manager) Put(intent *Intent) {
	if intent == nil {
		panic("cannot insert nil *Intent into IntentManager")
	}

	// bucket special-case collections
	if intent.IsOplog() {
		manager.oplogIntent = intent
		manager.specialIntents[intent.Namespace()] = intent
		return
	}
	if intent.IsSystemIndexes() {
		manager.indexIntents[intent.DB] = intent
		manager.specialIntents[intent.Namespace()] = intent
		return
	}
	if intent.IsUsers() {
		if intent.BSONFile != nil {
			manager.usersIntent = intent
			manager.specialIntents[intent.Namespace()] = intent
		}
		return
	}
	if intent.IsRoles() {
		if intent.BSONFile != nil {
			manager.rolesIntent = intent
			manager.specialIntents[intent.Namespace()] = intent
		}
		return
	}
	if intent.IsAuthVersion() {
		if intent.BSONFile != nil {
			manager.versionIntent = intent
			manager.specialIntents[intent.Namespace()] = intent
		}
		return
	}

	// BSON and metadata files for the same collection are merged
	// into the same intent. This is done to allow for simple
	// pairing of BSON + metadata without keeping track of the
	// state of the filepath walker
	if existing := manager.intents[intent.Namespace()]; existing != nil {
		// merge new intent into old intent
		if existing.BSONFile == nil {
			existing.BSONFile = intent.BSONFile
		}
		if existing.Size == 0 {
			existing.Size = intent.Size
		}
		if existing.Location == "" {
			existing.Location = intent.Location
		}
		if existing.MetadataFile == nil {
			existing.MetadataFile = intent.MetadataFile
		}
		if existing.MetadataLocation == "" {
			existing.MetadataLocation = intent.MetadataLocation
		}
		return
	}

	// if key doesn't already exist, add it to the manager
	manager.intents[intent.Namespace()] = intent
	manager.intentsByDiscoveryOrder = append(manager.intentsByDiscoveryOrder, intent)
}

// Intents returns a slice containing all of the intents in the manager.
// Intents is not thread safe
func (manager *Manager) Intents() []*Intent {
	allIntents := []*Intent{}
	for _, intent := range manager.intents {
		allIntents = append(allIntents, intent)
	}
	for _, intent := range manager.indexIntents {
		allIntents = append(allIntents, intent)
	}
	if manager.oplogIntent != nil {
		allIntents = append(allIntents, manager.oplogIntent)
	}
	if manager.usersIntent != nil {
		allIntents = append(allIntents, manager.usersIntent)
	}
	if manager.rolesIntent != nil {
		allIntents = append(allIntents, manager.rolesIntent)
	}
	if manager.versionIntent != nil {
		allIntents = append(allIntents, manager.versionIntent)
	}
	return allIntents
}

func (manager *Manager) IntentForNamespace(ns string) *Intent {
	intent := manager.intents[ns]
	if intent != nil {
		return intent
	}
	intent = manager.specialIntents[ns]
	return intent
}

// Pop returns the next available intent from the manager. If the manager is
// empty, it returns nil. Pop is thread safe.
func (manager *Manager) Pop() *Intent {
	manager.priotitizerLock.Lock()
	defer manager.priotitizerLock.Unlock()

	intent := manager.prioritizer.Get()
	return intent
}

// Peek returns a copy of a stored intent from the manager without removing
// the intent. This method is useful for edge cases that need to look ahead
// at what collections are in the manager before they are scheduled.
//
// NOTE: There are no guarantees that peek will return a usable
// intent after Finalize() is called.
func (manager *Manager) Peek() *Intent {
	if len(manager.intentsByDiscoveryOrder) == 0 {
		return nil
	}
	intentCopy := *manager.intentsByDiscoveryOrder[0]
	return &intentCopy
}

// Finish tells the prioritizer that mongorestore is done restoring
// the given collection intent.
func (manager *Manager) Finish(intent *Intent) {
	manager.priotitizerLock.Lock()
	defer manager.priotitizerLock.Unlock()
	manager.prioritizer.Finish(intent)
}

// Oplog returns the intent representing the oplog, which isn't
// stored with the other intents, because it is backuped and restored in
// a very different way from other collections.
func (manager *Manager) Oplog() *Intent {
	return manager.oplogIntent
}

// SystemIndexes returns the system.indexes bson for a database
func (manager *Manager) SystemIndexes(dbName string) *Intent {
	return manager.indexIntents[dbName]
}

// SystemIndexes returns the databases for which there are system.indexes
func (manager *Manager) SystemIndexDBs() []string {
	databases := []string{}
	for dbname := range manager.indexIntents {
		databases = append(databases, dbname)
	}
	return databases
}

// Users returns the intent of the users collection to restore, a special case
func (manager *Manager) Users() *Intent {
	return manager.usersIntent
}

// Roles returns the intent of the user roles collection to restore, a special case
func (manager *Manager) Roles() *Intent {
	return manager.rolesIntent
}

// AuthVersion returns the intent of the version collection to restore, a special case
func (manager *Manager) AuthVersion() *Intent {
	return manager.versionIntent
}

// Finalize processes the intents for prioritization. Currently only two
// kinds of prioritizers are supported. No more "Put" operations may be done
// after finalize is called.
func (manager *Manager) Finalize(pType PriorityType) {
	switch pType {
	case Legacy:
		log.Log(log.DebugHigh, "finalizing intent manager with legacy prioritizer")
		manager.prioritizer = NewLegacyPrioritizer(manager.intentsByDiscoveryOrder)
	case LongestTaskFirst:
		log.Log(log.DebugHigh, "finalizing intent manager with longest task first prioritizer")
		manager.prioritizer = NewLongestTaskFirstPrioritizer(manager.intentsByDiscoveryOrder)
	case MultiDatabaseLTF:
		log.Log(log.DebugHigh, "finalizing intent manager with multi-database longest task first prioritizer")
		manager.prioritizer = NewMultiDatabaseLTFPrioritizer(manager.intentsByDiscoveryOrder)
	default:
		panic("cannot initialize IntentPrioritizer with unknown type")
	}
	// release these for the garbage collector and to ensure code correctness
	manager.intents = nil
	manager.intentsByDiscoveryOrder = nil
}

func (manager *Manager) UsePrioritizer(prioritizer IntentPrioritizer) {
	manager.prioritizer = prioritizer
}
