package store

import (
	"os"
	"reflect"

	"strings"

	"sourcegraph.com/sourcegraph/srclib/unit"
)

// IndexStatus describes an index and its status (whether it exists,
// etc.).
type IndexStatus struct {
	// Repo is the ID of the repository this index pertains to. If it
	// pertains to all repositories in a MultiRepoStore, or if it
	// pertains to the current (and only) repository in a RepoStore or
	// lower-level store, the Repo field is empty.
	Repo string `json:",omitempty"`

	// CommitID is the commit ID of the version this index pertains to. If it
	// pertains to all commits in a RepoStore, or if it
	// pertains to the current (and only) commit in a TreeStore or
	// lower-level store, the CommitID field is empty.
	CommitID string `json:",omitempty"`

	// Unit is the commit ID of the version this index pertains to. If
	// it pertains to all units in a TreeStore, or if it pertains to
	// the current (and only) source unit in a UnitStore, the Unit
	// field is empty.
	Unit *unit.ID2 `json:",omitempty"`

	// Stale is a boolean value indicating whether the index needs to
	// be (re)built.
	Stale bool

	// Name is the name of the index.
	Name string

	// Type is the type of the index.
	Type string

	// Size is the length in bytes of the index if it is a regular
	// file.
	Size int64 `json:",omitempty"`

	// Error is the error encountered while determining this index's
	// status, if any.
	Error string `json:",omitempty"`
}

// IndexCriteria restricts the list of indexes returned by Indexes to
// only those that match the criteria. Non-empty conditions are ANDed
// together.
type IndexCriteria struct {
	Repo     string
	CommitID string
	Unit     *unit.ID2
	Name     string
	Type     string
	Stale    *bool
}

// Indexes returns a list of indexes and their statuses for s and its
// lower-level stores.  If indexChan is non-nil, it receives indexes
// as soon as they are found; when all matching indexes have been
// found, the func returns and all indexes are included in the
// returned slice.
func Indexes(store interface{}, c IndexCriteria, indexChan chan<- IndexStatus) ([]IndexStatus, error) {
	var xs []IndexStatus
	indexChan2 := make(chan IndexStatus)
	go func() {
		for x := range indexChan2 {
			xs = append(xs, x)
			if indexChan != nil {
				indexChan <- x
			}
		}
	}()
	err := listIndexes(store, c, indexChan2, nil)
	close(indexChan2)
	return xs, err
}

// listIndexes lists indexes in s (a store) asynchronously, sending
// status objects to ch. If f != nil, it is called to set/modify
// fields on each status object before the status object is sent to
// the channel.
func listIndexes(s interface{}, c IndexCriteria, ch chan<- IndexStatus, f func(*IndexStatus)) error {
	switch s := s.(type) {
	case indexedStore:
		xx := s.Indexes()
		for name, x := range xx {
			st := IndexStatus{
				Name: name,
				Type: strings.TrimPrefix(reflect.TypeOf(x).String(), "*store."),
			}

			if !strings.Contains(st.Name, c.Name) {
				continue
			}
			if !strings.Contains(st.Type, c.Type) {
				continue
			}

			fi, err := s.statIndex(name)
			if os.IsNotExist(err) {
				st.Stale = true
			} else if err != nil {
				st.Error = err.Error()
			} else {
				st.Size = fi.Size()
			}

			if c.Stale != nil && st.Stale != *c.Stale {
				continue
			}

			if f != nil {
				f(&st)
			}
			ch <- st
		}

		switch s := s.(type) {
		case *indexedTreeStore:
			if err := listIndexes(s.fsTreeStore, c, ch, f); err != nil {
				return err
			}
		case *indexedUnitStore:
			if err := listIndexes(s.fsUnitStore, c, ch, f); err != nil {
				return err
			}
		}

	case repoStoreOpener:
		var rss map[string]RepoStore
		if c.Repo == "" {
			var err error
			rss, err = s.openAllRepoStores()
			if err != nil && !isStoreNotExist(err) {
				return err
			}
		} else {
			rss = map[string]RepoStore{c.Repo: s.openRepoStore(c.Repo)}
		}
		for repo, rs := range rss {
			err := listIndexes(rs, c, ch, func(x *IndexStatus) {
				x.Repo = repo
				if f != nil {
					f(x)
				}
			})
			if err != nil {
				return err
			}
		}

	case treeStoreOpener:
		var tss map[string]TreeStore
		if c.CommitID == "" {
			var err error
			tss, err = s.openAllTreeStores()
			if err != nil && !isStoreNotExist(err) {
				return err
			}
		} else {
			tss = map[string]TreeStore{c.CommitID: s.openTreeStore(c.CommitID)}
		}
		for commitID, ts := range tss {
			err := listIndexes(ts, c, ch, func(x *IndexStatus) {
				x.CommitID = commitID
				if f != nil {
					f(x)
				}
			})
			if err != nil {
				return err
			}
		}

	case unitStoreOpener:
		var uss map[unit.ID2]UnitStore
		if c.Unit == nil {
			var err error
			uss, err = s.openAllUnitStores()
			if err != nil && !isStoreNotExist(err) {
				return err
			}
		} else {
			uss = map[unit.ID2]UnitStore{*c.Unit: s.openUnitStore(*c.Unit)}
		}
		for unit, us := range uss {
			unitCopy := unit
			err := listIndexes(us, c, ch, func(x *IndexStatus) {
				x.Unit = &unitCopy
				if f != nil {
					f(x)
				}
			})
			if err != nil {
				return err
			}
		}

	}
	return nil
}